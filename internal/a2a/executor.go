package a2a

import (
	"context"
	"iter"
	"log/slog"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Stable identifiers for the single response artifact, mirroring the TS
// RESPONSE_ARTIFACT_ID / RESPONSE_ARTIFACT_NAME. A fixed id lets streamed text
// deltas append to one artifact rather than creating a new one per chunk.
const (
	responseArtifactID   a2a.ArtifactID = "response"
	responseArtifactName                = "response"
)

// Executor implements [a2asrv.AgentExecutor]: it translates one agent run into
// the A2A v1.0 event sequence (submitted → working → artifact(s) → terminal
// status). The library owns everything else — task store, SSE framing, the
// JSON-RPC dispatch. Protocol validation of the message shape happens in the
// library; assistant-specific validation (projectName, non-empty text) happens
// here and surfaces as ErrInvalidParams.
type Executor struct {
	runner AgentRunner
	logger *slog.Logger
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// NewExecutor returns an [Executor] driving runner. A nil logger is silenced.
func NewExecutor(runner AgentRunner, logger *slog.Logger) *Executor {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Executor{runner: runner, logger: logger}
}

// Execute runs one turn and emits its A2A events. Errors yielded before any
// event become JSON-RPC errors (e.g. ErrInvalidParams → -32602); once events
// flow, a failure is reported as a failed status-update, not an error.
func (e *Executor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		userText := UserText(ec.Message)
		projectName := ProjectName(ec.Message, ec.Metadata)

		// Assistant-level validation. projectName is the metering/authz key and
		// is required; the message must carry some text to act on.
		if projectName == "" {
			yield(nil, a2a.NewError(a2a.ErrInvalidParams,
				"message.metadata.projectName is required (the project the task runs against)"))
			return
		}
		if userText == "" {
			yield(nil, a2a.NewError(a2a.ErrInvalidParams,
				"message.parts must include at least one non-empty text part"))
			return
		}

		// First event MUST be a Task (the library rejects a non-Task first
		// event). For a brand-new message there is no stored task yet. Stamp
		// projectName into the task metadata so the server's authz middleware
		// can gate later tasks/get and tasks/cancel on the owning project.
		if ec.StoredTask == nil {
			task := a2a.NewSubmittedTask(ec, ec.Message)
			task.SetMeta(projectNameKey, projectName)
			if !yield(task, nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}

		sink := &streamSink{ec: ec, yield: yield}
		result := e.runner.Run(ctx, RunRequest{
			UserText:    userText,
			ProjectName: projectName,
			ContextID:   ec.ContextID,
			TaskID:      string(ec.TaskID),
			Mentions:    Mentions(ec.Message, ec.Metadata),
		}, sink)
		if sink.stopped {
			return
		}

		switch result.State {
		case RunCanceled:
			e.logger.Info("a2a.task.canceled", "taskId", string(ec.TaskID), "project", projectName)
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)

		case RunFailed:
			msg := "Agent run failed"
			if result.Error != "" {
				msg = "Agent run failed: " + result.Error
			}
			e.logger.Error("a2a.task.failed", "taskId", string(ec.TaskID), "project", projectName, "error", result.Error)
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed, agentMessage(ec, msg)), nil)

		default: // RunCompleted
			// If the model produced no streamed text, still emit the (possibly
			// empty) answer as a single artifact so clients see a well-formed
			// artifact — but never an empty-parts artifact (the library rejects
			// it), so skip when there is truly nothing to say.
			if !sink.emitted && result.Text != "" {
				if !yield(responseArtifactEvent(ec, result.Text), nil) {
					return
				}
			}
			var finalMsg *a2a.Message
			if result.Text != "" {
				finalMsg = agentMessage(ec, result.Text)
			}
			e.logger.Info("a2a.task.finalized", "taskId", string(ec.TaskID), "project", projectName, "state", "completed")
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, finalMsg), nil)
		}
	}
}

// Cancel emits a canceled status-update. The library handles not-found and
// not-cancelable (terminal-state) cases before Cancel is ever reached.
func (e *Executor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// streamSink turns streamed text deltas into A2A artifact events: the first
// delta creates the "response" artifact, later deltas append to it. Tool
// activity goes out alongside, as working status updates (see activity.go).
type streamSink struct {
	ec         *a2asrv.ExecutorContext
	yield      func(a2a.Event, error) bool
	artifactID a2a.ArtifactID
	emitted    bool
	stopped    bool
}

func (s *streamSink) OnTextDelta(text string) {
	if s.stopped || text == "" {
		return
	}
	var ev *a2a.TaskArtifactUpdateEvent
	if !s.emitted {
		ev = responseArtifactEvent(s.ec, text)
		s.artifactID = ev.Artifact.ID
		s.emitted = true
	} else {
		ev = a2a.NewArtifactUpdateEvent(s.ec, s.artifactID, a2a.NewTextPart(text))
	}
	if !s.yield(ev, nil) {
		s.stopped = true
	}
}

// OnToolStart and OnToolFinish announce one tool call's lifecycle as
// working-state status updates carrying a [toolActivityData] data part. They
// keep the task in the working state — the terminal status is still the
// executor's to emit — so a client that ignores the data part sees nothing
// new.
func (s *streamSink) OnToolStart(act ToolActivity) {
	s.emitActivity(toolActivityData{
		Kind: toolActivityKind, Phase: toolPhaseStarted,
		ID: act.ID, Name: act.Name, Summary: act.Summary,
	})
}

func (s *streamSink) OnToolFinish(act ToolActivity) {
	s.emitActivity(toolActivityData{
		Kind: toolActivityKind, Phase: toolPhaseFinished,
		ID: act.ID, Name: act.Name, Summary: act.Summary,
		OK: act.OK, ElapsedMs: act.Elapsed.Milliseconds(),
	})
}

func (s *streamSink) emitActivity(data toolActivityData) {
	if s.stopped || data.Name == "" {
		return
	}
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, s.ec, a2a.NewDataPart(data))
	if !s.yield(a2a.NewStatusUpdateEvent(s.ec, a2a.TaskStateWorking, msg), nil) {
		s.stopped = true
	}
}

// responseArtifactEvent builds a fresh (non-append) artifact event carrying text
// under the stable "response" artifact id/name.
func responseArtifactEvent(ec *a2asrv.ExecutorContext, text string) *a2a.TaskArtifactUpdateEvent {
	info := ec.TaskInfo()
	return &a2a.TaskArtifactUpdateEvent{
		TaskID:    info.TaskID,
		ContextID: info.ContextID,
		Artifact: &a2a.Artifact{
			ID:    responseArtifactID,
			Name:  responseArtifactName,
			Parts: a2a.ContentParts{a2a.NewTextPart(text)},
		},
	}
}

func agentMessage(ec *a2asrv.ExecutorContext, text string) *a2a.Message {
	return a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(text))
}
