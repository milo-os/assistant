package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/usage"
)

// AgentAttributionName is the fixed agent identity sent in the x-datum-agent
// attribution header (gateway mode only).
const AgentAttributionName = "patch"

// DefaultMaxOutputTokens is the per-request output-token cap applied when
// [Deps].MaxOutputTokens is unset. It matches the TypeScript service, which
// always sent 4096; agentcore itself imposes no default (0 defers to the
// provider), so this service-level policy lives here.
const DefaultMaxOutputTokens = 4096

// DefaultHistoryTokenBudget caps the estimated tokens of replayed history per
// turn when [Deps].HistoryTokenBudget is unset. Replayed turns are billed
// input tokens on every subsequent request, so an unbounded conversation
// would grow cost quadratically; oldest turns are dropped first.
const DefaultHistoryTokenBudget = 6000

// State is the terminal state of a conversation task.
type State string

const (
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
)

// Deps are the injected collaborators a [Conversation] needs. They are set
// once from configuration; none is read from the environment here.
type Deps struct {
	// Model is the resolved language model (mock, anthropic, or gateway).
	Model agentcore.Model
	// ModelMode is "mock", "anthropic", or "gateway". It gates the gateway
	// attribution headers; those are never sent in other modes.
	ModelMode string
	// Source supplies a project's capability documents. Nil means the
	// assistant runs with no provider capabilities.
	Source capability.Source
	// Emitter delivers usage events. Required (a no-op emitter is fine).
	Emitter *usage.Emitter
	// HTTPClient fetches provider knowledge. Nil uses the default client.
	HTTPClient *http.Client
	// History replays and records conversation turns per (project, contextId),
	// making follow-up messages in the same A2A context conversational. Nil
	// disables memory: every turn is answered standalone.
	History history.Store
	// AllowPrivateCapabilityNetworks relaxes the capability SSRF guard's
	// loopback/RFC1918 block (link-local/metadata stay blocked either way). The
	// platform's capability endpoints are in-cluster private ClusterIPs, so real
	// deployments set this true; it is threaded into capability.Compose.
	AllowPrivateCapabilityNetworks bool
	// StepLimit and MaxOutputTokens override the loop defaults when > 0.
	// HistoryTokenBudget overrides DefaultHistoryTokenBudget when > 0.
	StepLimit          int
	MaxOutputTokens    int
	HistoryTokenBudget int
	// Logger receives orchestration logs. Nil discards them.
	Logger *slog.Logger
}

// Conversation runs conversational tasks against a fixed set of dependencies.
// It is safe for concurrent use: each [Conversation.Run] is independent.
type Conversation struct {
	deps   Deps
	logger *slog.Logger
}

// New constructs a [Conversation].
func New(deps Deps) *Conversation {
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Conversation{deps: deps, logger: logger}
}

// Params identifies one task to run.
type Params struct {
	// UserText is the user's message.
	UserText string
	// ProjectName scopes capabilities and metering. Empty disables metering.
	ProjectName string
	// ContextID is the A2A contextId == conversation id == metering resource.
	ContextID string
	// TaskID is the A2A task id, used for logging.
	TaskID string
}

// EventKind discriminates streamed [Event]s.
type EventKind string

const (
	// EventText is an incremental fragment of the assistant's answer.
	EventText EventKind = "text-delta"
	// EventToolCall announces that the model invoked a provider tool.
	EventToolCall EventKind = "tool-call"
)

// Event is one streamed happening during a run. The A2A layer translates these
// into SSE status updates.
type Event struct {
	Kind     EventKind
	Text     string
	ToolName string
}

// UsageSummary is the run's aggregated token usage and metering outcome.
type UsageSummary struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadTokens          int64
	CacheWriteTokens         int64
	TokenEventCount          int
	ToolInvocationEventCount int
	// Emitted is true when a collector was configured and accepted the batch.
	Emitted bool
}

// Result is the terminal outcome of a run, available from [Stream.Result]
// after the stream reaches io.EOF.
type Result struct {
	State       State
	Text        string
	Error       string
	Usage       UsageSummary
	UsageEvents []usage.Event
}

// Run starts a conversation task and returns a [Stream] of its events. The
// caller drains the stream with Recv until io.EOF, then reads [Stream.Result].
// Composition always happens; the MCP sessions are always closed and usage is
// always metered (best-effort) when the stream finalizes.
func (c *Conversation) Run(ctx context.Context, params Params) *Stream {
	docs := c.loadDocuments(ctx, params)

	var (
		mu          sync.Mutex
		invocations []capability.ProviderToolInvocation
	)
	composed, _ := capability.Compose(ctx, docs, capability.ComposeOptions{
		HTTPClient:           c.deps.HTTPClient,
		AllowPrivateNetworks: c.deps.AllowPrivateCapabilityNetworks,
		OnToolInvocation: func(inv capability.ProviderToolInvocation) {
			mu.Lock()
			invocations = append(invocations, inv)
			mu.Unlock()
			c.logger.Info("agent.tool.invoked",
				"taskId", params.TaskID, "projectName", params.ProjectName,
				"service", inv.ServiceName, "tool", inv.NamespacedToolName)
		},
		Logger: c.logger,
	})

	maxOutputTokens := c.deps.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = DefaultMaxOutputTokens
	}
	messages := append(history.Messages(c.loadHistory(ctx, params)),
		agentcore.UserMessage(params.UserText))
	inner := agentcore.Run(ctx, agentcore.LoopOptions{
		Model:           c.deps.Model,
		System:          BuildSystemPrompt(composed.SystemPromptAddendum),
		Messages:        messages,
		Tools:           composed.Tools,
		StepLimit:       c.deps.StepLimit,
		MaxOutputTokens: maxOutputTokens,
		Headers:         attributionHeaders(c.deps.ModelMode, params.ProjectName, params.ContextID),
	})

	return &Stream{
		ctx:         ctx,
		conv:        c,
		params:      params,
		inner:       inner,
		composed:    composed,
		invocations: &invocations,
		invMu:       &mu,
		state:       StateCompleted,
	}
}

// loadHistory returns the conversation's prior turns, truncated to the token
// budget. Any store failure degrades to an empty memory (the turn is still
// answered) rather than failing the chat.
func (c *Conversation) loadHistory(ctx context.Context, params Params) []history.Turn {
	if c.deps.History == nil || params.ContextID == "" {
		return nil
	}
	turns, err := c.deps.History.Turns(ctx, params.ProjectName, params.ContextID)
	if err != nil {
		c.logger.Warn("agent.history.load_failed",
			"projectName", params.ProjectName, "contextId", params.ContextID, "error", err.Error())
		return nil
	}
	budget := c.deps.HistoryTokenBudget
	if budget <= 0 {
		budget = DefaultHistoryTokenBudget
	}
	return history.Truncate(turns, budget)
}

// loadDocuments fetches the project's capability documents, degrading to none
// (the built-in-only assistant) on any source failure rather than failing the
// chat.
func (c *Conversation) loadDocuments(ctx context.Context, params Params) []capability.CapabilityDocument {
	if c.deps.Source == nil {
		return nil
	}
	docs, err := c.deps.Source.Documents(ctx, params.ProjectName)
	if err != nil {
		c.logger.Warn("agent.documents.load_failed",
			"projectName", params.ProjectName, "error", err.Error())
		return nil
	}
	return docs
}

// attributionHeaders returns the gateway attribution headers, or nil in any
// non-gateway mode (we never leak project/conversation ids to a real provider).
func attributionHeaders(mode, projectName, contextID string) map[string]string {
	if mode != "gateway" {
		return nil
	}
	return map[string]string{
		"x-datum-project":      projectName,
		"x-datum-conversation": contextID,
		"x-datum-agent":        AgentAttributionName,
	}
}

// Stream is the event stream of one running conversation. It also accumulates
// the terminal [Result], available after Recv reports io.EOF.
type Stream struct {
	ctx         context.Context
	conv        *Conversation
	params      Params
	inner       agentcore.StreamReader
	composed    *capability.Composed
	invocations *[]capability.ProviderToolInvocation
	invMu       *sync.Mutex

	text      strings.Builder
	total     agentcore.Usage
	state     State
	errMsg    string
	result    *Result
	finalized bool
}

// Recv returns the next user-facing [Event], or io.EOF once the run is
// complete. On io.EOF the run has been finalized (sessions closed, usage
// metered) and [Stream.Result] is ready.
func (s *Stream) Recv() (Event, error) {
	for {
		part, err := s.inner.Recv()
		if err == io.EOF {
			s.finalize()
			return Event{}, io.EOF
		}
		if err != nil {
			s.state = StateFailed
			s.errMsg = err.Error()
			s.finalize()
			return Event{}, io.EOF
		}

		switch part.Kind {
		case agentcore.StreamPartTextDelta:
			s.text.WriteString(part.Text)
			return Event{Kind: EventText, Text: part.Text}, nil
		case agentcore.StreamPartToolCall:
			if part.ToolCall != nil {
				return Event{Kind: EventToolCall, ToolName: part.ToolCall.Name}, nil
			}
		case agentcore.StreamPartFinish:
			s.total = part.TotalUsage
			s.state = stateFromReason(part.FinishReason)
		case agentcore.StreamPartError:
			// A failed or canceled run still carries the usage of the steps
			// that completed before it broke, so those inferences are metered
			// (the provider billed them). Keep a cancellation distinct from a
			// genuine failure end to end.
			s.total = part.TotalUsage
			if part.FinishReason == agentcore.FinishCanceled {
				s.state = StateCanceled
			} else {
				s.state = StateFailed
			}
			if part.Err != nil {
				s.errMsg = part.Err.Error()
			}
		default:
			// tool-result and step-finish are internal to the run.
		}
	}
}

// Result returns the terminal outcome. It is only valid after Recv has
// returned io.EOF.
func (s *Stream) Result() Result {
	if s.result == nil {
		return Result{State: s.state, Text: s.text.String(), Error: s.errMsg}
	}
	return *s.result
}

// Close releases the stream's resources. It is safe to call more than once and
// finalizes the run (metering + session close) if that has not happened yet.
func (s *Stream) Close() error {
	_ = s.inner.Close()
	s.finalize()
	return nil
}

// finalize closes the MCP sessions and meters usage exactly once. It runs
// after the loop has fully drained, so the invocation list is complete.
func (s *Stream) finalize() {
	if s.finalized {
		return
	}
	s.finalized = true

	_ = s.composed.Close()

	events := s.buildUsageEvents()

	// Emit even if the task context was canceled — metering is terminal and
	// best-effort, and must not be skipped just because the run was aborted.
	res := s.conv.deps.Emitter.Emit(context.WithoutCancel(s.ctx), events)

	s.recordHistory()

	s.result = &Result{
		State:       s.state,
		Text:        s.text.String(),
		Error:       s.errMsg,
		UsageEvents: events,
		Usage: UsageSummary{
			InputTokens:              s.total.Input,
			OutputTokens:             s.total.Output,
			CacheReadTokens:          s.total.CacheRead,
			CacheWriteTokens:         s.total.CacheWrite,
			TokenEventCount:          countTokenEvents(events),
			ToolInvocationEventCount: len(*s.invocations),
			Emitted:                  res.OK && !res.Noop,
		},
	}
}

// recordHistory appends the finished exchange to conversation memory. Only a
// completed turn with an actual answer is worth remembering: a failed or
// canceled run produced nothing the user saw as an answer, and recording it
// would replay a half-exchange into every later prompt.
func (s *Stream) recordHistory() {
	store := s.conv.deps.History
	if store == nil || s.params.ContextID == "" || s.state != StateCompleted {
		return
	}
	answer := s.text.String()
	if answer == "" {
		return
	}
	err := store.Append(context.WithoutCancel(s.ctx), s.params.ProjectName, s.params.ContextID,
		history.Turn{UserText: s.params.UserText, AssistantText: answer})
	if err != nil {
		s.conv.logger.Warn("agent.history.append_failed",
			"projectName", s.params.ProjectName, "contextId", s.params.ContextID, "error", err.Error())
	}
}

// buildUsageEvents builds the token meters plus one tool-invocation event per
// provider tool call. Nothing is billed without a project.
func (s *Stream) buildUsageEvents() []usage.Event {
	if s.params.ProjectName == "" {
		return nil
	}
	events := usage.BuildUsageEvents(usage.BuildUsageInput{
		ProjectName:    s.params.ProjectName,
		ConversationID: s.params.ContextID,
		Model:          s.conv.deps.Model.ModelID(),
		Tokens: usage.UsageTokens{
			InputTokens:              s.total.Input,
			OutputTokens:             s.total.Output,
			CachedInputTokens:        s.total.CacheRead,
			CacheCreationInputTokens: s.total.CacheWrite,
		},
	})
	s.invMu.Lock()
	invocations := append([]capability.ProviderToolInvocation(nil), *s.invocations...)
	s.invMu.Unlock()
	for _, inv := range invocations {
		events = append(events, usage.BuildToolInvocationEvent(usage.BuildToolInvocationInput{
			ProjectName:    s.params.ProjectName,
			ConversationID: s.params.ContextID,
			ServiceName:    inv.ServiceName,
		}))
	}
	return events
}

func countTokenEvents(events []usage.Event) int {
	n := 0
	for _, e := range events {
		if !strings.HasSuffix(e.MeterName, "tool-invocations") {
			n++
		}
	}
	return n
}

func stateFromReason(reason agentcore.FinishReason) State {
	if reason == agentcore.FinishCanceled {
		return StateCanceled
	}
	return StateCompleted
}
