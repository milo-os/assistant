package main

import (
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// capture is a test Io that records the two streams.
type capture struct {
	out strings.Builder
	err strings.Builder
}

func (c *capture) Out(text string) { c.out.WriteString(text) }
func (c *capture) Err(text string) { c.err.WriteString(text) }

// seqOf builds a streaming iterator from a fixed list of events.
func seqOf(events ...a2a.Event) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for _, e := range events {
			if !yield(e, nil) {
				return
			}
		}
	}
}

func textPart(s string) *a2a.Part { return a2a.NewTextPart(s) }

func TestRenderChat_Completed(t *testing.T) {
	events := seqOf(
		&a2a.Task{ID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}},
		&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
		&a2a.TaskArtifactUpdateEvent{TaskID: "t", ContextID: "c", Artifact: &a2a.Artifact{ID: "response", Parts: a2a.ContentParts{textPart("Findings: CONSUMER_LAG")}}},
		&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}},
	)
	var io capture
	code, err := renderChat(events, false, &io)
	if err != nil {
		t.Fatalf("unexpected stream err: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	// The answer text goes to stdout, ending with a tidy newline.
	if !strings.Contains(io.out.String(), "CONSUMER_LAG") {
		t.Errorf("stdout missing answer: %q", io.out.String())
	}
	if !strings.HasSuffix(io.out.String(), "\n") {
		t.Errorf("stdout should end with newline: %q", io.out.String())
	}
	// Status transitions go to stderr, not stdout.
	if !strings.Contains(io.err.String(), "working") || !strings.Contains(io.err.String(), "completed") {
		t.Errorf("stderr missing transitions: %q", io.err.String())
	}
	if strings.Contains(io.out.String(), "working") {
		t.Errorf("status leaked into stdout: %q", io.out.String())
	}
}

func TestRenderChat_JSON(t *testing.T) {
	events := seqOf(
		&a2a.Task{ID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}},
		&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}},
	)
	var io capture
	code, err := renderChat(events, true, &io)
	if err != nil {
		t.Fatalf("unexpected stream err: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	// --json emits raw events to stdout, one per line, and no stderr decoration.
	if io.err.String() != "" {
		t.Errorf("stderr should be empty in json mode: %q", io.err.String())
	}
	lines := strings.Split(strings.TrimSpace(io.out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 json lines, got %d: %q", len(lines), io.out.String())
	}
	for _, l := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Errorf("line is not valid JSON: %q (%v)", l, err)
		}
	}
}

func TestRenderChat_Failed(t *testing.T) {
	failMsg := a2a.NewMessage(a2a.MessageRoleAgent, textPart("Agent run failed: boom"))
	events := seqOf(
		&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
		&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateFailed, Message: failMsg}},
	)
	var io capture
	code, err := renderChat(events, false, &io)
	if err != nil {
		t.Fatalf("unexpected stream err: %v", err)
	}
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(io.err.String(), "failed") || !strings.Contains(io.err.String(), "boom") {
		t.Errorf("stderr missing failure detail: %q", io.err.String())
	}
}

func TestRenderChat_StreamError(t *testing.T) {
	boom := errors.New("stream exploded")
	events := func(yield func(a2a.Event, error) bool) {
		yield(&a2a.TaskStatusUpdateEvent{TaskID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}, nil)
		yield(nil, boom)
	}
	var io capture
	code, err := renderChat(events, false, &io)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !errors.Is(err, boom) {
		t.Errorf("want stream error surfaced, got %v", err)
	}
}

func TestRenderCard(t *testing.T) {
	card := &a2a.AgentCard{
		Name:        "Patch",
		Description: "desc",
		Version:     "0.1.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface("http://x/a2a", a2a.TransportProtocolJSONRPC),
		},
		Provider:     &a2a.AgentProvider{Org: "Datum", URL: "http://datum"},
		Capabilities: a2a.AgentCapabilities{Streaming: true},
		SecuritySchemes: a2a.NamedSecuritySchemes{
			"bearer": a2a.HTTPAuthSecurityScheme{Scheme: "Bearer"},
		},
		Skills: []a2a.AgentSkill{{ID: "project-assistant", Name: "PA", Description: "d"}},
	}

	var pretty capture
	renderCard(card, false, &pretty)
	for _, want := range []string{"Patch", "http://x/a2a", "JSONRPC", "project-assistant", "http bearer", "Datum"} {
		if !strings.Contains(pretty.out.String(), want) {
			t.Errorf("pretty card missing %q:\n%s", want, pretty.out.String())
		}
	}

	var asJSON capture
	renderCard(card, true, &asJSON)
	var v map[string]any
	if err := json.Unmarshal([]byte(asJSON.out.String()), &v); err != nil {
		t.Fatalf("card json invalid: %v", err)
	}
	if v["name"] != "Patch" {
		t.Errorf("card json name = %v, want Patch", v["name"])
	}
}

func TestRenderTask(t *testing.T) {
	task := &a2a.Task{
		ID:        "t-1",
		ContextID: "c-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{ID: "response", Parts: a2a.ContentParts{textPart("the answer")}}},
	}
	var io capture
	renderTask(task, false, &io)
	for _, want := range []string{"t-1", "completed", "the answer"} {
		if !strings.Contains(io.out.String(), want) {
			t.Errorf("task render missing %q:\n%s", want, io.out.String())
		}
	}
}
