package patchcli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// activityEvent builds the working status update the service sends for one
// tool-activity phase, round-tripped through JSON so the test decodes exactly
// what arrives over the wire (a data part is a map[string]any there, not the
// service's struct).
func activityEvent(t *testing.T, payload map[string]any) *a2a.TaskStatusUpdateEvent {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewDataPart(decoded))
	return &a2a.TaskStatusUpdateEvent{
		TaskID: "t", ContextID: "c",
		Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: msg},
	}
}

func startedEvent(t *testing.T, name, summary string) *a2a.TaskStatusUpdateEvent {
	return activityEvent(t, map[string]any{
		"kind": "tool_call", "phase": "started", "id": "call-1", "name": name, "summary": summary,
	})
}

func finishedEvent(t *testing.T, name string, ok bool, elapsedMs int) *a2a.TaskStatusUpdateEvent {
	return activityEvent(t, map[string]any{
		"kind": "tool_call", "phase": "finished", "id": "call-1", "name": name,
		"ok": ok, "elapsedMs": elapsedMs,
	})
}

func TestToolActivityFrom_DecodesDataPart(t *testing.T) {
	act, ok := toolActivityFrom(finishedEvent(t, "list_workloads", true, 1234).Status.Message)
	if !ok {
		t.Fatal("a tool_call data part should decode")
	}
	if act.Name != "list_workloads" || !act.OK || act.started() {
		t.Errorf("decoded activity = %+v", act)
	}
	if act.elapsed().Milliseconds() != 1234 {
		t.Errorf("elapsed = %s, want 1.234s", act.elapsed())
	}
}

func TestToolActivityFrom_IgnoresOtherMessages(t *testing.T) {
	if _, ok := toolActivityFrom(nil); ok {
		t.Error("a nil message carries no activity")
	}
	plain := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Agent run failed: boom"))
	if _, ok := toolActivityFrom(plain); ok {
		t.Error("a text-only status message carries no activity")
	}
	other := activityEvent(t, map[string]any{"kind": "something_else", "name": "x"})
	if _, ok := toolActivityFrom(other.Status.Message); ok {
		t.Error("a data part of another kind must not be read as activity")
	}
}

func TestRenderChat_ActivityRowsGoToStderr(t *testing.T) {
	events := seqOf(
		&a2a.Task{ID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}},
		&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
		startedEvent(t, "list_workloads", "project=demo"),
		finishedEvent(t, "list_workloads", true, 1200),
		&a2a.TaskArtifactUpdateEvent{TaskID: "t", ContextID: "c", Artifact: &a2a.Artifact{ID: "response", Parts: a2a.ContentParts{textPart("all good")}}},
		&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}},
	)
	var io capture
	code, err := renderChat(events, false, &io)
	if err != nil || code != 0 {
		t.Fatalf("renderChat = (%d, %v)", code, err)
	}
	if !strings.Contains(io.err.String(), "• list_workloads … 1.2s") {
		t.Errorf("stderr missing the completed activity row: %q", io.err.String())
	}
	if strings.Contains(io.out.String(), "list_workloads") {
		t.Errorf("activity leaked into stdout: %q", io.out.String())
	}
	// An activity update is progress within "working", not another transition.
	if strings.Count(io.err.String(), "» working") != 1 {
		t.Errorf("activity should not print extra state lines: %q", io.err.String())
	}
}

func TestRenderChat_FailedActivityRow(t *testing.T) {
	events := seqOf(
		finishedEvent(t, "load_skill", false, 400),
		&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}},
	)
	var io capture
	if _, err := renderChat(events, false, &io); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(io.err.String(), "• load_skill … failed · 0.4s") {
		t.Errorf("stderr missing the failed activity row: %q", io.err.String())
	}
}

// --json stays a raw event dump: no activity rows, nothing on stderr.
func TestRenderChat_ActivityInJSONMode(t *testing.T) {
	events := seqOf(
		startedEvent(t, "list_workloads", "project=demo"),
		&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}},
	)
	var io capture
	if _, err := renderChat(events, true, &io); err != nil {
		t.Fatal(err)
	}
	if io.err.String() != "" {
		t.Errorf("--json should write nothing to stderr, got %q", io.err.String())
	}
	if !strings.Contains(io.out.String(), `"tool_call"`) {
		t.Errorf("--json should emit the raw activity event: %q", io.out.String())
	}
}
