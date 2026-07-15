package agentcore

import (
	"context"
	"io"
	"strings"
)

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

// runStep runs one model inference and consumes its stream, returning the
// accumulated text, the requested tool calls, the step usage, and the
// step's finish reason. ok is false when an error or cancellation terminated
// the run (a terminal part was already emitted).
func (r *run) runStep(messages []Message, toolDefs []ToolDefinition) (text string, toolCalls []ToolCall, usage Usage, reason FinishReason, ok bool) {
	ms, err := r.opts.Model.Stream(r.ctx, Request{
		System:          r.opts.System,
		Messages:        messages,
		Tools:           toolDefs,
		MaxOutputTokens: r.opts.MaxOutputTokens,
		Headers:         r.opts.Headers,
	})
	if err != nil {
		r.emitError(err)
		return "", nil, Usage{}, "", false
	}
	defer ms.Close()

	var textBuf strings.Builder
	reason = FinishStop
	for {
		part, err := ms.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			r.emitError(err)
			return "", nil, Usage{}, "", false
		}

		switch part.Kind {
		case StreamPartTextDelta:
			textBuf.WriteString(part.Text)
			if !r.emit(part) {
				return "", nil, Usage{}, "", false
			}
		case StreamPartToolCall:
			if part.ToolCall != nil {
				toolCalls = append(toolCalls, *part.ToolCall)
			}
			if !r.emit(part) {
				return "", nil, Usage{}, "", false
			}
		case StreamPartStepFinish:
			usage = part.Usage
			reason = part.FinishReason
		default:
			// Adapters only produce the three kinds above; ignore anything
			// else defensively rather than corrupting the run.
		}
	}

	return textBuf.String(), toolCalls, usage, reason, true
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

func (r *run) emitError(err error) {
	r.emit(StreamPart{Kind: StreamPartError, FinishReason: FinishError, Err: err})
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
