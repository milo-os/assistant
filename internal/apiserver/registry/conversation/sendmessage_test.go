package conversation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"

	"github.com/milo-os/assistant/internal/a2a"
)

// fakeRunner is a scripted a2a.AgentRunner: it replays a fixed sequence of
// sink callbacks, then returns the configured terminal result. It also
// records the request it was driven with, so tests can assert the Connecter
// filled in project/context/task fields correctly.
type fakeRunner struct {
	textDeltas []string
	toolStarts []a2a.ToolActivity
	toolFinish []a2a.ToolActivity
	result     a2a.RunResult

	lastReq a2a.RunRequest
}

func (f *fakeRunner) Run(_ context.Context, req a2a.RunRequest, sink a2a.RunSink) a2a.RunResult {
	f.lastReq = req
	for _, d := range f.textDeltas {
		sink.OnTextDelta(d)
	}
	for _, act := range f.toolStarts {
		sink.OnToolStart(act)
	}
	for _, act := range f.toolFinish {
		sink.OnToolFinish(act)
	}
	return f.result
}

// doConnect drives Connect end-to-end against an httptest recorder, decoding
// the resulting SSE stream into one JSON object per frame.
func doConnect(t *testing.T, rest *SendMessageREST, ctx context.Context, name, body string) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()
	handler, err := rest.Connect(ctx, name, nil, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var events []map[string]any
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimPrefix(line, "data: ")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("frame %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return rec, events
}

func TestSendMessageConnect_CompletedRun(t *testing.T) {
	runner := &fakeRunner{
		textDeltas: []string{"Hello", " world"},
		toolStarts: []a2a.ToolActivity{{ID: "c1", Name: "list_workloads", Summary: "project=demo"}},
		toolFinish: []a2a.ToolActivity{{ID: "c1", Name: "list_workloads", OK: true, Elapsed: 5 * time.Millisecond}},
		result:     a2a.RunResult{State: a2a.RunCompleted, Text: "Hello world"},
	}
	rest := NewSendMessageREST(runner)
	ctx := request.WithNamespace(context.Background(), "demo")

	rec, events := doConnect(t, rest, ctx, "ctx-1", `{"text":"hi there"}`)

	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(events), events)
	}
	if events[0]["type"] != "text_delta" || events[0]["text"] != "Hello" {
		t.Errorf("events[0] = %+v", events[0])
	}
	if events[1]["type"] != "text_delta" || events[1]["text"] != " world" {
		t.Errorf("events[1] = %+v", events[1])
	}
	if events[2]["type"] != "tool_start" || events[2]["name"] != "list_workloads" || events[2]["summary"] != "project=demo" {
		t.Errorf("events[2] = %+v", events[2])
	}
	if events[3]["type"] != "tool_finish" || events[3]["ok"] != true {
		t.Errorf("events[3] = %+v", events[3])
	}
	last := events[4]
	if last["type"] != "done" || last["state"] != "completed" || last["text"] != "Hello world" {
		t.Errorf("events[4] (done) = %+v", last)
	}
	if _, hasErr := last["error"]; hasErr {
		t.Errorf("done event carries an error field on a completed run: %+v", last)
	}

	if runner.lastReq.ProjectName != "demo" || runner.lastReq.ContextID != "ctx-1" {
		t.Errorf("run request = %+v, want project=demo context=ctx-1", runner.lastReq)
	}
	if runner.lastReq.UserText != "hi there" {
		t.Errorf("UserText = %q", runner.lastReq.UserText)
	}
	if runner.lastReq.TaskID == "" {
		t.Error("TaskID was not generated")
	}
}

func TestSendMessageConnect_FailedRun(t *testing.T) {
	runner := &fakeRunner{result: a2a.RunResult{State: a2a.RunFailed, Error: "model unavailable"}}
	rest := NewSendMessageREST(runner)
	ctx := request.WithNamespace(context.Background(), "demo")

	_, events := doConnect(t, rest, ctx, "ctx-1", `{"text":"hi"}`)

	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (done only): %+v", len(events), events)
	}
	if events[0]["type"] != "done" || events[0]["state"] != "failed" || events[0]["error"] != "model unavailable" {
		t.Errorf("done event = %+v", events[0])
	}
}

func TestSendMessageConnect_MentionsPassedThrough(t *testing.T) {
	runner := &fakeRunner{result: a2a.RunResult{State: a2a.RunCompleted}}
	rest := NewSendMessageREST(runner)
	ctx := request.WithNamespace(context.Background(), "demo")

	doConnect(t, rest, ctx, "ctx-1", `{"text":"hi","mentions":[{"kind":"Workload","name":"api"}]}`)

	if len(runner.lastReq.Mentions) != 1 || runner.lastReq.Mentions[0].Name != "api" {
		t.Errorf("mentions = %+v", runner.lastReq.Mentions)
	}
}

// recordingResponder captures the object/error the Connecter's inner handler
// reports through rest.Responder, for asserting the bad-request paths that
// never reach the SSE portion of the handler body.
type recordingResponder struct {
	err error
}

func (r *recordingResponder) Object(int, runtime.Object) {}
func (r *recordingResponder) Error(err error)            { r.err = err }

func TestSendMessageConnect_MissingTextIsBadRequest(t *testing.T) {
	rest := NewSendMessageREST(&fakeRunner{})
	ctx := request.WithNamespace(context.Background(), "demo")
	responder := &recordingResponder{}
	handler, err := rest.Connect(ctx, "ctx-1", nil, responder)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"text":"   "}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !apierrors.IsBadRequest(responder.err) {
		t.Fatalf("responder.err = %v, want BadRequest (blank text)", responder.err)
	}
}

func TestSendMessageConnect_UnknownNamespaceIsBadRequest(t *testing.T) {
	rest := NewSendMessageREST(&fakeRunner{})
	_, err := rest.Connect(context.Background(), "ctx-1", nil, nil)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest (no namespace)", err)
	}
}

func TestSendMessageConnect_InvalidNameIsBadRequest(t *testing.T) {
	rest := NewSendMessageREST(&fakeRunner{})
	ctx := request.WithNamespace(context.Background(), "demo")
	_, err := rest.Connect(ctx, "foo\x00bar", nil, nil)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest (invalid name)", err)
	}
}

func TestSendMessageConnect_NilRunnerIsServiceUnavailable(t *testing.T) {
	rest := NewSendMessageREST(nil)
	ctx := request.WithNamespace(context.Background(), "demo")
	_, err := rest.Connect(ctx, "ctx-1", nil, nil)
	if err == nil || !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("err = %v, want ServiceUnavailable", err)
	}
}

func TestSendMessageREST_ConnectMethodsIsPostOnly(t *testing.T) {
	rest := NewSendMessageREST(&fakeRunner{})
	methods := rest.ConnectMethods()
	if len(methods) != 1 || methods[0] != http.MethodPost {
		t.Fatalf("ConnectMethods = %v, want [POST]", methods)
	}
}
