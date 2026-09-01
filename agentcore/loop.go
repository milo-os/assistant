package agentcore

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// errConsumerGone is an internal sentinel: an attempt stops early because the
// stream's consumer closed it (emit returned false). It is never surfaced to
// the caller and never triggers a retry — the run simply unwinds.
var errConsumerGone = errors.New("agentcore: stream consumer closed")

// DefaultStepLimit is the number of model steps (tool-call rounds) [Run]
// allows before it stops gracefully. It is used when [LoopOptions].StepLimit
// is zero.
const DefaultStepLimit = 8

// LoopOptions configures a single run of the tool-use loop. Model and the
// initial Messages are required; everything else has a sensible default.
type LoopOptions struct {
	// Model is the language model to drive. Required.
	Model Model
	// System is the system prompt applied to every step. Optional.
	System string
	// Messages is the initial conversation (typically a single user
	// message). The loop appends assistant and tool messages to a copy of
	// this slice as it iterates; the caller's slice is not mutated.
	Messages []Message
	// Tools are the executable tools available to the model. A nil or empty
	// set runs the model with no tools (it can then only produce text).
	Tools ToolSet
	// StepLimit caps the number of model steps. Zero means
	// [DefaultStepLimit]. Reaching the limit ends the run with
	// [FinishStepLimit] (loop rule 3).
	StepLimit int
	// MaxOutputTokens is forwarded to the model on every step. Zero lets the
	// adapter choose its provider default.
	MaxOutputTokens int
	// Headers are extra HTTP headers attached to every model request (used
	// for gateway attribution). Optional.
	Headers map[string]string
	// OnStep, if set, is called with each step's usage immediately after the
	// step finishes and before the next step starts. It exposes per-step
	// usage for billing (loop rule 4); the aggregate is also delivered on the
	// terminal [StreamPartFinish].
	OnStep func(Usage)
	// MaxRetries is the number of additional attempts made when a step fails
	// with a RETRYABLE error (rate limit, overload, transient transport) and
	// before any output has streamed. Zero means [DefaultMaxRetries]; a
	// negative value disables retries entirely.
	MaxRetries int
	// RetryBaseDelay is the base of the exponential retry backoff. Zero means
	// [DefaultRetryBaseDelay].
	RetryBaseDelay time.Duration
	// RetryMaxDelay caps a single backoff wait (a server Retry-After is still
	// honored verbatim). Zero means [DefaultRetryMaxDelay].
	RetryMaxDelay time.Duration
}

// Run drives opts.Model through the tool-use loop and returns a
// [StreamReader] over the unified event stream. The stream begins emitting
// immediately from a background goroutine; the caller consumes it with Recv
// until [io.EOF] and MUST call Close when done (Close also cancels an
// in-progress run).
//
// The stream always ends with exactly one terminal part: a
// [StreamPartFinish] on success (carrying the aggregated [Usage]) or a
// [StreamPartError] on a model/transport failure. The loop obeys these
// rules:
//
//  1. It exits as soon as a step produces no tool calls — it does not key
//     off the raw stop reason. A step that hits its token limit with tool
//     calls still pending finishes explicitly ([FinishLength]) rather than
//     executing possibly-truncated calls.
//  2. All tool results from one step are batched into a single tool message
//     fed back to the model.
//  3. It stops gracefully at StepLimit with [FinishStepLimit].
//  4. It aggregates per-step usage into the total, never dropping cache
//     read/write, and reports per-step usage through OnStep.
//  5. A tool that errors, or an unknown tool name, becomes an error tool
//     result fed back to the model — never a loop abort.
//  6. It emits the unified [StreamPart] kinds (text delta, tool call, tool
//     result, step finish, finish, error).
func Run(ctx context.Context, opts LoopOptions) StreamReader {
	ctx, cancel := context.WithCancel(ctx)
	return StreamFunc(
		func(send SendFunc) {
			(&run{opts: opts, ctx: ctx, send: send}).drive()
		},
		cancel,
	)
}

// run holds the mutable state of a single loop execution.
type run struct {
	opts  LoopOptions
	ctx   context.Context
	send  SendFunc
	total Usage
}

// emit sends a part to the caller, returning false if the caller closed the
// stream. A false return means the producer goroutine should stop.
func (r *run) emit(part StreamPart) bool {
	return r.send(part)
}

func (r *run) drive() {
	stepLimit := r.opts.StepLimit
	if stepLimit <= 0 {
		stepLimit = DefaultStepLimit
	}

	messages := append([]Message(nil), r.opts.Messages...)
	toolDefs := r.opts.Tools.Definitions()

	for step := 0; ; step++ {
		if r.ctx.Err() != nil {
			r.emit(StreamPart{Kind: StreamPartFinish, FinishReason: FinishCanceled, TotalUsage: r.total})
			return
		}
		if step >= stepLimit {
			r.emit(StreamPart{Kind: StreamPartFinish, FinishReason: FinishStepLimit, TotalUsage: r.total})
			return
		}

		text, toolCalls, stepUsage, reason, ok := r.runStep(messages, toolDefs)
		if !ok {
			return // an error or cancellation was already emitted
		}

		r.total = r.total.Add(stepUsage)
		if !r.emit(StreamPart{Kind: StreamPartStepFinish, Usage: stepUsage, FinishReason: reason}) {
			return
		}
		if r.opts.OnStep != nil {
			r.opts.OnStep(stepUsage)
		}

		// Rule 1: no tool calls ends the loop; the step's own reason
		// (stop/length) is surfaced as the run's finish reason.
		if len(toolCalls) == 0 {
			r.emit(StreamPart{Kind: StreamPartFinish, FinishReason: reason, TotalUsage: r.total})
			return
		}
		// Rule 1 (explicit truncation case): tool calls are pending but the
		// step ran out of output budget, so they may be incomplete. Finish
		// rather than execute them.
		if reason == FinishLength {
			r.emit(StreamPart{Kind: StreamPartFinish, FinishReason: FinishLength, TotalUsage: r.total})
			return
		}

		messages = append(messages, assistantMessage(text, toolCalls))

		results, ok := r.executeTools(toolCalls)
		if !ok {
			return
		}
		// Rule 2: every tool result from this step goes back in ONE message.
		messages = append(messages, Message{Role: RoleTool, Content: results})
	}
}

// runStep runs one model step, retrying a retryable failure (rate limit,
// overload, transient transport) with bounded exponential backoff so a real
// provider's transient errors do not fail the turn. It returns the
// accumulated text, tool calls, step usage, and finish reason. ok is false
// when an error or cancellation terminated the run (a terminal part was
// already emitted).
//
// A retry is only possible BEFORE any output has streamed: once a delta or a
// tool call has reached the caller the answer is committed, so a later failure
// is surfaced (never silently retried into a fresh, duplicate answer). A
// failed attempt never produced a step-finish, so its usage is discarded here
// and only the successful attempt's usage is billed by [run.drive].
func (r *run) runStep(messages []Message, toolDefs []ToolDefinition) (text string, toolCalls []ToolCall, usage Usage, reason FinishReason, ok bool) {
	maxRetries := r.opts.MaxRetries
	if maxRetries == 0 {
		maxRetries = DefaultMaxRetries
	} else if maxRetries < 0 {
		maxRetries = 0
	}

	for attempt := 0; ; attempt++ {
		text, toolCalls, usage, reason, streamed, err := r.runAttempt(messages, toolDefs)
		if err == nil {
			return text, toolCalls, usage, reason, true
		}
		if errors.Is(err, errConsumerGone) {
			return "", nil, Usage{}, "", false // consumer closed; unwind silently
		}
		// Retry only a retryable failure that struck before any output
		// committed and only while attempts remain.
		if !streamed && attempt < maxRetries && isRetryable(err) {
			if r.backoff(attempt, err) {
				continue
			}
			// The context was canceled/expired while backing off.
			r.emitError(r.ctx.Err())
			return "", nil, Usage{}, "", false
		}
		r.emitError(err)
		return "", nil, Usage{}, "", false
	}
}

// runAttempt runs one model inference and consumes its stream. streamed
// reports whether any text delta or tool call reached the caller (which makes
// the attempt non-retryable). err is non-nil on a failed/canceled stream; it
// is [errConsumerGone] when the caller closed the stream mid-attempt.
func (r *run) runAttempt(messages []Message, toolDefs []ToolDefinition) (text string, toolCalls []ToolCall, usage Usage, reason FinishReason, streamed bool, err error) {
	ms, serr := r.opts.Model.Stream(r.ctx, Request{
		System:          r.opts.System,
		Messages:        messages,
		Tools:           toolDefs,
		MaxOutputTokens: r.opts.MaxOutputTokens,
		Headers:         r.opts.Headers,
	})
	if serr != nil {
		return "", nil, Usage{}, "", false, serr
	}
	defer ms.Close()

	var textBuf strings.Builder
	reason = FinishStop
	for {
		part, rerr := ms.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", nil, Usage{}, "", streamed, rerr
		}

		switch part.Kind {
		case StreamPartTextDelta:
			textBuf.WriteString(part.Text)
			streamed = true
			if !r.emit(part) {
				return "", nil, Usage{}, "", streamed, errConsumerGone
			}
		case StreamPartToolCall:
			if part.ToolCall != nil {
				toolCalls = append(toolCalls, *part.ToolCall)
			}
			streamed = true
			if !r.emit(part) {
				return "", nil, Usage{}, "", streamed, errConsumerGone
			}
		case StreamPartStepFinish:
			usage = part.Usage
			reason = part.FinishReason
		case StreamPartError:
			// Every adapter emits StreamPartError to signal a failed or
			// canceled model stream. It MUST propagate as an attempt failure —
			// falling through here would report a truncated stream as a
			// normal stop-completion (billing a half-answer as success).
			return "", nil, Usage{}, "", streamed, part.Err
		default:
			// No adapter emits the remaining kinds mid-stream; ignore them
			// defensively rather than corrupting the run.
		}
	}

	return textBuf.String(), toolCalls, usage, reason, streamed, nil
}

// backoff sleeps before the given retry attempt, returning false if the run's
// context is canceled or its per-turn deadline expires while waiting (so the
// caller can end the run as canceled rather than retry a dead request).
func (r *run) backoff(attempt int, err error) bool {
	d := retryDelay(attempt, err, r.opts.RetryBaseDelay, r.opts.RetryMaxDelay)
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.ctx.Done():
		return false
	}
}

// executeTools runs every tool call for a step and returns the results as
// tool-result content parts, emitting each as a [StreamPartToolResult]. ok
// is false only when the caller canceled the stream mid-execution.
func (r *run) executeTools(toolCalls []ToolCall) ([]ContentPart, bool) {
	results := make([]ContentPart, 0, len(toolCalls))
	for i := range toolCalls {
		res := r.executeTool(toolCalls[i])
		results = append(results, ContentPart{Kind: ContentToolResult, ToolResult: &res})
		if !r.emit(StreamPart{Kind: StreamPartToolResult, ToolResult: &res}) {
			return nil, false
		}
	}
	return results, true
}

// executeTool runs a single tool call, translating an unknown tool or an
// execution error into an error result (loop rule 5) rather than failing.
func (r *run) executeTool(call ToolCall) ToolResult {
	tool, found := r.opts.Tools[call.Name]
	if !found {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Content:    "unknown tool: " + call.Name,
			IsError:    true,
		}
	}
	out, err := tool.Execute(r.ctx, call.Input)
	if err != nil {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Content:    err.Error(),
			IsError:    true,
		}
	}
	return ToolResult{ToolCallID: call.ID, Name: call.Name, Content: out}
}

// emitError ends the run with the terminal error part. It carries the usage
// accumulated over the steps that completed before the failure (r.total) so a
// mid-run failure still bills the inferences the provider already ran, and it
// distinguishes a cancellation or per-turn deadline expiry ([FinishCanceled])
// from any other failure ([FinishError]) end to end.
func (r *run) emitError(err error) {
	reason := FinishError
	if isCancellation(err) || isCancellation(r.ctx.Err()) {
		reason = FinishCanceled
	}
	r.emit(StreamPart{Kind: StreamPartError, FinishReason: reason, Err: err, TotalUsage: r.total})
}

// assistantMessage builds the assistant turn recorded in history after a
// tool-calling step: the generated text (if any) followed by the tool calls.
func assistantMessage(text string, toolCalls []ToolCall) Message {
	content := make([]ContentPart, 0, len(toolCalls)+1)
	if text != "" {
		content = append(content, TextPart(text))
	}
	for i := range toolCalls {
		tc := toolCalls[i]
		content = append(content, ContentPart{Kind: ContentToolCall, ToolCall: &tc})
	}
	return Message{Role: RoleAssistant, Content: content}
}
