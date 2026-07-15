package a2a

import "context"

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
}

// RunResult is the terminal outcome of [AgentRunner.Run].
type RunResult struct {
	State RunState
	// Text is the final assistant answer (accumulated across streamed deltas).
	Text string
	// Error is a human-readable failure message when State is [RunFailed].
	Error string
}

// RunSink receives incremental output while an agent run is in progress. The
// executor's sink implementation translates each delta into an A2A artifact
// event.
type RunSink interface {
	// OnTextDelta is called for each chunk of generated assistant text.
	OnTextDelta(text string)
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
