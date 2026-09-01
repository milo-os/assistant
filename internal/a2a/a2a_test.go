package a2a

import (
	"context"
	"errors"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// scriptRunner is a fake AgentRunner with a fixed outcome and optional streamed
// deltas, for exercising the executor's event translation in isolation.
type scriptRunner struct {
	result RunResult
	deltas []string
}

func (s scriptRunner) Run(_ context.Context, _ RunRequest, sink RunSink) RunResult {
	for _, d := range s.deltas {
		sink.OnTextDelta(d)
	}
	return s.result
}

// collect drains an executor's event iterator into slices of events and errors.
func collect(exec *Executor, ec *a2asrv.ExecutorContext) ([]a2a.Event, []error) {
	var events []a2a.Event
	var errs []error
	for ev, err := range exec.Execute(context.Background(), ec) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		events = append(events, ev)
	}
	return events, errs
}

func execCtx(text, project string) *a2asrv.ExecutorContext {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	if project != "" {
		msg.SetMeta("projectName", project)
	}
	return &a2asrv.ExecutorContext{Message: msg, TaskID: "task-1", ContextID: "ctx-1"}
}

func lastStatus(t *testing.T, events []a2a.Event) *a2a.TaskStatusUpdateEvent {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if s, ok := events[i].(*a2a.TaskStatusUpdateEvent); ok {
			return s
		}
	}
	t.Fatal("no status-update event emitted")
	return nil
}

func TestExecute_CompletedStreamsArtifactsAndFinalStatus(t *testing.T) {
	exec := NewExecutor(scriptRunner{
		result: RunResult{State: RunCompleted, Text: "hello world"},
		deltas: []string{"hello ", "world"},
	}, nil)
	events, errs := collect(exec, execCtx("hi", "proj"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// First event must be the submitted Task, stamped with projectName.
	task, ok := events[0].(*a2a.Task)
	if !ok {
		t.Fatalf("first event = %T, want *a2a.Task", events[0])
	}
	if task.Metadata["projectName"] != "proj" {
		t.Errorf("submitted task missing projectName metadata: %+v", task.Metadata)
	}
	// Two artifact events (create + append) carrying the streamed text.
	var artifactText string
	artifactCount := 0
	for _, e := range events {
		if a, ok := e.(*a2a.TaskArtifactUpdateEvent); ok {
			artifactCount++
			for _, p := range a.Artifact.Parts {
				artifactText += p.Text()
			}
			if a.Artifact.ID != responseArtifactID {
				t.Errorf("artifact id = %q, want %q", a.Artifact.ID, responseArtifactID)
			}
		}
	}
	if artifactCount != 2 {
		t.Errorf("artifact events = %d, want 2 (create + append)", artifactCount)
	}
	if artifactText != "hello world" {
		t.Errorf("streamed artifact text = %q", artifactText)
	}
	final := lastStatus(t, events)
	if final.Status.State != a2a.TaskStateCompleted {
		t.Errorf("final state = %q", final.Status.State)
	}
	if final.Status.Message == nil || final.Status.Message.Parts[0].Text() != "hello world" {
		t.Errorf("final status message = %+v", final.Status.Message)
	}
}

func TestExecute_NoStreamedTextEmitsSingleArtifact(t *testing.T) {
	exec := NewExecutor(scriptRunner{result: RunResult{State: RunCompleted, Text: "answer"}}, nil)
	events, _ := collect(exec, execCtx("hi", "proj"))
	artifacts := 0
	for _, e := range events {
		if _, ok := e.(*a2a.TaskArtifactUpdateEvent); ok {
			artifacts++
		}
	}
	if artifacts != 1 {
		t.Errorf("artifact events = %d, want 1 (non-streamed answer)", artifacts)
	}
}

func TestExecute_FailedRunEmitsFailedStatus(t *testing.T) {
	exec := NewExecutor(scriptRunner{result: RunResult{State: RunFailed, Error: "boom"}}, nil)
	events, errs := collect(exec, execCtx("hi", "proj"))
	if len(errs) != 0 {
		t.Fatalf("failed run should surface as a failed status, not an error: %v", errs)
	}
	final := lastStatus(t, events)
	if final.Status.State != a2a.TaskStateFailed {
		t.Errorf("final state = %q, want failed", final.Status.State)
	}
	if final.Status.Message == nil || final.Status.Message.Parts[0].Text() != "Agent run failed: boom" {
		t.Errorf("failed message = %+v", final.Status.Message)
	}
}

func TestExecute_CanceledRunEmitsCanceledStatus(t *testing.T) {
	exec := NewExecutor(scriptRunner{result: RunResult{State: RunCanceled}}, nil)
	events, _ := collect(exec, execCtx("hi", "proj"))
	if lastStatus(t, events).Status.State != a2a.TaskStateCanceled {
		t.Error("want canceled final state")
	}
}

func TestExecute_MissingProjectNameYieldsInvalidParams(t *testing.T) {
	exec := NewExecutor(scriptRunner{result: RunResult{State: RunCompleted, Text: "x"}}, nil)
	events, errs := collect(exec, execCtx("hi", ""))
	if len(events) != 0 {
		t.Errorf("expected no events before the validation error, got %d", len(events))
	}
	if len(errs) != 1 || !errors.Is(errs[0], a2a.ErrInvalidParams) {
		t.Fatalf("want a single ErrInvalidParams, got %v", errs)
	}
}

func TestExecute_MissingTextYieldsInvalidParams(t *testing.T) {
	exec := NewExecutor(scriptRunner{result: RunResult{State: RunCompleted}}, nil)
	// A message whose only part is empty text.
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(""))
	msg.SetMeta("projectName", "proj")
	ec := &a2asrv.ExecutorContext{Message: msg, TaskID: "t", ContextID: "c"}
	_, errs := collect(exec, ec)
	if len(errs) != 1 || !errors.Is(errs[0], a2a.ErrInvalidParams) {
		t.Fatalf("want ErrInvalidParams, got %v", errs)
	}
}

func TestCancel_EmitsCanceledStatus(t *testing.T) {
	exec := NewExecutor(scriptRunner{}, nil)
	ec := execCtx("hi", "proj")
	var got a2a.TaskState
	for ev, err := range exec.Cancel(context.Background(), ec) {
		if err == nil {
			if s, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
				got = s.Status.State
			}
		}
	}
	if got != a2a.TaskStateCanceled {
		t.Errorf("cancel emitted %q, want canceled", got)
	}
}

// ── extraction helpers ────────────────────────────────────────

func TestProjectName(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.SetMeta("projectName", " demo ")
	if got := ProjectName(msg, nil); got != "demo" {
		t.Errorf("message metadata: got %q, want demo (trimmed)", got)
	}

	// Falls back to params metadata.
	bare := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	if got := ProjectName(bare, map[string]any{"projectName": "fromParams"}); got != "fromParams" {
		t.Errorf("params fallback: got %q", got)
	}

	if got := ProjectName(bare, nil); got != "" {
		t.Errorf("absent: got %q, want empty", got)
	}
}

func TestUserText(t *testing.T) {
	msg := &a2a.Message{Parts: a2a.ContentParts{
		a2a.NewTextPart("hello"),
		a2a.NewDataPart(map[string]any{"k": "v"}), // non-text ignored
		a2a.NewTextPart("world"),
	}}
	if got := UserText(msg); got != "hello world" {
		t.Errorf("UserText = %q, want 'hello world'", got)
	}
	if got := UserText(nil); got != "" {
		t.Errorf("nil message: got %q", got)
	}
}
