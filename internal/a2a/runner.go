package a2a

import (
	"context"
	"errors"
	"time"
)

// RunState is the terminal outcome of an agent run.
type RunState string

const (
	// RunCompleted means the agent produced a final answer.
	RunCompleted RunState = "completed"
	// RunFailed means the run errored.
	RunFailed RunState = "failed"
	// RunCanceled means the run was canceled (via context cancellation).
	RunCanceled RunState = "canceled"
)

// RunRequest is one conversation turn to run for a task.
type RunRequest struct {
	UserText    string
	ProjectName string
	// ContextID is the A2A contextId == conversation id == metering resource.
	ContextID string
	TaskID    string
	// Mentions are the resources the user pointed at with "@kind/name". They
	// are advisory context for the turn, already present verbatim in UserText.
	Mentions []Mention
}

// RunResult is the terminal outcome of [AgentRunner.Run].
type RunResult struct {
	State RunState
	// Text is the final assistant answer (accumulated across streamed deltas).
	Text string
	// Error is a human-readable failure message when State is [RunFailed].
	Error string
}

// ToolActivity is one tool invocation as the client should see it: enough to
// render an activity row, and nothing else. Summary is already redacted and
// capped by [SummarizeToolInput] — raw tool arguments never reach this struct,
// because everything in it is streamed to the client.
type ToolActivity struct {
	// ID correlates the started and finished halves of one call. Empty when
	// the provider assigned no tool-call id.
	ID string
	// Name is the model-facing tool name (a provider tool, or a built-in such
	// as load_skill).
	Name string
	// Summary is a short human-readable argument line, e.g. "project=demo".
	Summary string
	// OK reports success; meaningful on the finished half only.
	OK bool
	// Elapsed is how long the call took; meaningful on the finished half only.
	Elapsed time.Duration
}

// RunSink receives incremental output while an agent run is in progress. The
// executor's sink implementation translates each delta into an A2A artifact
// event and each tool lifecycle callback into a working status update.
type RunSink interface {
	// OnTextDelta is called for each chunk of generated assistant text.
	OnTextDelta(text string)
	// OnToolStart is called when the model asks for a tool (including the
	// built-in load_skill, which is how a skill load becomes visible).
	OnToolStart(act ToolActivity)
	// OnToolFinish is called once the call has run, with its outcome and
	// elapsed time.
	OnToolFinish(act ToolActivity)
}

// AgentRunner drives one conversation turn: it composes capabilities, runs the
// model/tool loop, emits usage events, streams incremental text via sink, and
// returns the terminal outcome. It is the seam between this A2A surface and the
// agent-orchestration layer (internal/agent); cancellation flows through ctx.
//
// This interface is defined consumer-side so the orchestration package need not
// import this one; cmd/assistant adapts the concrete implementation to it.
type AgentRunner interface {
	Run(ctx context.Context, req RunRequest, sink RunSink) RunResult
}

// CompactRequest identifies one manual, user-triggered history compaction
// (the "/compact" command), as opposed to a [RunRequest] turn.
type CompactRequest struct {
	ProjectName string
	// ContextID is the A2A contextId == conversation id whose history is
	// being compacted.
	ContextID string
}

// ErrNothingToCompact is returned by [Compactor.Compact] when the target
// conversation has no history to fold — empty, or already reduced to a
// single summary turn by a prior compaction. It is a sentinel at this layer
// (distinct from, but mapped from, internal/agent's own ErrNothingToCompact)
// so this package and internal/server never need to import internal/agent
// just to recognize the case — the concrete AgentRunner implementation
// (cmd/assistant) does that mapping.
var ErrNothingToCompact = errors.New("a2a: nothing to compact")

// Compactor is implemented by an [AgentRunner] that also supports manual
// history compaction. It is a separate, optional interface — not folded into
// AgentRunner — because not every runner needs to support it (fakes used in
// tests of the run path have no reason to).
type Compactor interface {
	Compact(ctx context.Context, req CompactRequest) error
}
