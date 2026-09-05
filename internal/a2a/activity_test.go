package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// activityRunner is a fake AgentRunner that drives the sink's tool lifecycle
// callbacks, for exercising the activity → A2A event mapping in isolation.
type activityRunner struct {
	start  []ToolActivity
	finish []ToolActivity
	deltas []string
}

func (r activityRunner) Run(_ context.Context, _ RunRequest, sink RunSink) RunResult {
	for _, a := range r.start {
		sink.OnToolStart(a)
	}
	for _, d := range r.deltas {
		sink.OnTextDelta(d)
	}
	for _, a := range r.finish {
		sink.OnToolFinish(a)
	}
	return RunResult{State: RunCompleted, Text: strings.Join(r.deltas, "")}
}

// activityParts decodes every tool-activity data part carried by the events,
// through JSON so the test sees exactly what a client would.
func activityParts(t *testing.T, events []a2a.Event) []toolActivityData {
	t.Helper()
	var out []toolActivityData
	for _, ev := range events {
		s, ok := ev.(*a2a.TaskStatusUpdateEvent)
		if !ok || s.Status.Message == nil {
			continue
		}
		for _, p := range s.Status.Message.Parts {
			data := p.Data()
			if data == nil {
				continue
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatalf("marshal data part: %v", err)
			}
			var act toolActivityData
			if err := json.Unmarshal(raw, &act); err != nil {
				t.Fatalf("unmarshal data part: %v", err)
			}
			if act.Kind != toolActivityKind {
				t.Fatalf("data part kind = %q, want %q", act.Kind, toolActivityKind)
			}
			if s.Status.State != a2a.TaskStateWorking {
				t.Errorf("activity event state = %q, want working", s.Status.State)
			}
			out = append(out, act)
		}
	}
	return out
}

func TestExecute_ToolActivityBecomesWorkingDataParts(t *testing.T) {
	exec := NewExecutor(activityRunner{
		start:  []ToolActivity{{ID: "call-1", Name: "list_workloads", Summary: "project=demo"}},
		finish: []ToolActivity{{ID: "call-1", Name: "list_workloads", OK: true, Elapsed: 1234 * time.Millisecond}},
		deltas: []string{"here you go"},
	}, nil)

	events, errs := collect(exec, execCtx("hi", "proj"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	acts := activityParts(t, events)
	if len(acts) != 2 {
		t.Fatalf("activity data parts = %d, want 2 (started + finished): %+v", len(acts), acts)
	}
	if acts[0].Phase != toolPhaseStarted || acts[0].Name != "list_workloads" || acts[0].Summary != "project=demo" {
		t.Errorf("started event = %+v", acts[0])
	}
	if acts[0].ID != "call-1" || acts[1].ID != "call-1" {
		t.Errorf("ids should correlate the two halves: %+v / %+v", acts[0], acts[1])
	}
	if acts[1].Phase != toolPhaseFinished || !acts[1].OK || acts[1].ElapsedMs != 1234 {
		t.Errorf("finished event = %+v", acts[1])
	}
	// Text streaming is untouched by the activity events.
	var text string
	for _, ev := range events {
		if a, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
			for _, p := range a.Artifact.Parts {
				text += p.Text()
			}
		}
	}
	if text != "here you go" {
		t.Errorf("streamed artifact text = %q, want the answer unchanged", text)
	}
	if final := lastStatus(t, events); final.Status.State != a2a.TaskStateCompleted {
		t.Errorf("final state = %q, want completed", final.Status.State)
	}
}

func TestExecute_FailedToolActivityReportsNotOK(t *testing.T) {
	exec := NewExecutor(activityRunner{
		finish: []ToolActivity{{ID: "c", Name: "load_skill", OK: false, Elapsed: 400 * time.Millisecond}},
	}, nil)
	acts := activityParts(t, mustEvents(t, exec))
	if len(acts) != 1 {
		t.Fatalf("activity data parts = %d, want 1", len(acts))
	}
	if acts[0].OK || acts[0].ElapsedMs != 400 || acts[0].Name != "load_skill" {
		t.Errorf("finished event = %+v", acts[0])
	}
}

// A nameless activity has nothing to show, so it must not reach the client.
func TestExecute_NamelessActivityIsDropped(t *testing.T) {
	exec := NewExecutor(activityRunner{start: []ToolActivity{{ID: "c"}}}, nil)
	if acts := activityParts(t, mustEvents(t, exec)); len(acts) != 0 {
		t.Fatalf("activity data parts = %d, want 0: %+v", len(acts), acts)
	}
}

func mustEvents(t *testing.T, exec *Executor) []a2a.Event {
	t.Helper()
	events, errs := collect(exec, execCtx("hi", "proj"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	return events
}

func TestSummarizeToolInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"not json", "not json at all", ""},
		{"scalars sorted", `{"project":"demo","region":"us-east"}`, "project=demo, region=us-east"},
		{"numbers and bools", `{"limit":10,"watch":true}`, "limit=10, watch=true"},
		{"nested values elided", `{"filter":{"a":1},"tags":["x"],"name":"web"}`, "name=web"},
		{"at most three keys", `{"a":1,"b":2,"c":3,"d":4}`, "a=1, b=2, c=3"},
		{"credential keys redacted", `{"apiKey":"sk-live-123","name":"web"}`,
			"apiKey=[redacted], name=web"},
		{"password redacted", `{"password":"hunter2"}`, "password=[redacted]"},
		{"skill load", `{"skill":"streamco__lag-triage"}`, "skill=streamco__lag-triage"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SummarizeToolInput(json.RawMessage(tc.input)); got != tc.want {
				t.Errorf("SummarizeToolInput(%s) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSummarizeToolInput_CapsLength(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := SummarizeToolInput(json.RawMessage(`{"a":"` + long + `","b":"` + long + `","c":"` + long + `"}`))
	if len([]rune(got)) > maxSummaryLen {
		t.Errorf("summary length = %d runes, want <= %d: %q", len([]rune(got)), maxSummaryLen, got)
	}
	if strings.Contains(got, strings.Repeat("x", maxSummaryValueLen+1)) {
		t.Errorf("a single value escaped its cap: %q", got)
	}
}
