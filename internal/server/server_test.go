package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
)

// fakeRunner is a scriptable [assistanta2a.AgentRunner] standing in for the
// model/tool loop: it streams its reply as two text deltas and echoes a
// findings marker when asked to diagnose, mirroring the e2e mock model.
type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, req assistanta2a.RunRequest, sink assistanta2a.RunSink) assistanta2a.RunResult {
	var text string
	if strings.Contains(strings.ToLower(req.UserText), "diagnose") {
		text = "Pipeline p-1 findings: CONSUMER_LAG detected."
	} else {
		text = "Patch here — how can I help with this project?"
	}
	// Stream in two chunks to exercise artifact create + append.
	mid := len(text) / 2
	sink.OnTextDelta(text[:mid])
	sink.OnTextDelta(text[mid:])
	return assistanta2a.RunResult{State: assistanta2a.RunCompleted, Text: text}
}

const (
	goodToken = "good"
	wrongToken = "wrong"
	project    = "demo-project"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg, err := config.Load(config.MapGetenv(map[string]string{
		"AUTH_MODE":       "dev",
		"AUTH_DEV_TOKENS": goodToken + ":alice:" + project + ";" + wrongToken + ":bob:other-project",
		"MODEL_MODE":      "mock",
		"PUBLIC_BASE_URL": "http://assistant.test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	log := logger.Silent()
	app := New(Deps{
		Config:        cfg,
		Logger:        log,
		Authenticator: mustAuthenticator(t, cfg, log),
		Authorizer:    auth.NewAuthorizer(cfg, log),
		Runner:        fakeRunner{},
	})
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return srv
}

func mustAuthenticator(t *testing.T, cfg *config.Config, log *slog.Logger) auth.Authenticator {
	t.Helper()
	a, err := auth.NewAuthenticator(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// ── request helpers ───────────────────────────────────────────

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// task decodes the result as an [a2a.Task]. SendMessage wraps its result in a
// StreamResponse oneOf ({"task": …}); GetTask and CancelTask return a bare Task.
func (r rpcResponse) task(t *testing.T) *a2a.Task {
	t.Helper()
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(r.Result, &probe); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if _, wrapped := probe["task"]; wrapped {
		var sr a2a.StreamResponse
		if err := json.Unmarshal(r.Result, &sr); err != nil {
			t.Fatalf("decode StreamResponse: %v", err)
		}
		task, ok := sr.Event.(*a2a.Task)
		if !ok {
			t.Fatalf("wrapped result is %T, want *a2a.Task", sr.Event)
		}
		return task
	}
	var task a2a.Task
	if err := json.Unmarshal(r.Result, &task); err != nil {
		t.Fatalf("decode bare Task: %v", err)
	}
	return &task
}

func sendMessageParams(project, text string) map[string]any {
	msg := map[string]any{
		"messageId": "m-1",
		"role":      "ROLE_USER",
		"parts":     []any{map[string]any{"text": text}},
	}
	if project != "" {
		msg["metadata"] = map[string]any{"projectName": project}
	}
	return map[string]any{"message": msg}
}

func rpc(t *testing.T, srv *httptest.Server, token, method string, params any, id any) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/a2a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func decodeRPC(t *testing.T, res *http.Response) rpcResponse {
	t.Helper()
	defer res.Body.Close()
	var r rpcResponse
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		t.Fatalf("decode rpc response: %v", err)
	}
	return r
}

// ── health + card ─────────────────────────────────────────────

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	res, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestAgentCard(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/.well-known/agent-card.json", "/.well-known/agent.json"} {
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var card a2a.AgentCard
		if err := json.NewDecoder(res.Body).Decode(&card); err != nil {
			t.Fatalf("%s: decode card: %v", path, err)
		}
		res.Body.Close()
		if card.Name != "Patch" {
			t.Errorf("%s: name = %q", path, card.Name)
		}
		if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].URL != "http://assistant.test/a2a" {
			t.Errorf("%s: interfaces = %+v", path, card.SupportedInterfaces)
		}
		if card.SupportedInterfaces[0].ProtocolVersion != a2a.Version {
			t.Errorf("%s: protocol version = %q", path, card.SupportedInterfaces[0].ProtocolVersion)
		}
		if !card.Capabilities.Streaming {
			t.Errorf("%s: streaming capability should be true", path)
		}
		if _, ok := card.SecuritySchemes["bearer"]; !ok {
			t.Errorf("%s: bearer security scheme missing", path)
		}
		hasSkill := false
		for _, s := range card.Skills {
			if s.ID == "project-assistant" {
				hasSkill = true
			}
		}
		if !hasSkill {
			t.Errorf("%s: project-assistant skill missing", path)
		}
	}
}

// ── auth ──────────────────────────────────────────────────────

func TestAuth(t *testing.T) {
	srv := newTestServer(t)

	t.Run("401 without token", func(t *testing.T) {
		res := rpc(t, srv, "", "SendMessage", sendMessageParams(project, "hi"), 1)
		defer res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("status = %d", res.StatusCode)
		}
		if res.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("missing WWW-Authenticate header")
		}
	})

	t.Run("401 unknown token", func(t *testing.T) {
		res := rpc(t, srv, "nope", "SendMessage", sendMessageParams(project, "hi"), 1)
		defer res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("status = %d", res.StatusCode)
		}
	})

	t.Run("403 ungranted project", func(t *testing.T) {
		res := rpc(t, srv, wrongToken, "SendMessage", sendMessageParams(project, "hi"), 1)
		defer res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatalf("status = %d", res.StatusCode)
		}
	})

	t.Run("200 good token granted project", func(t *testing.T) {
		res := rpc(t, srv, goodToken, "SendMessage", sendMessageParams(project, "hi"), 1)
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("status = %d", res.StatusCode)
		}
	})
}

// ── SendMessage ───────────────────────────────────────────────

func TestSendMessage(t *testing.T) {
	srv := newTestServer(t)
	res := rpc(t, srv, goodToken, "SendMessage", sendMessageParams(project, "Diagnose pipeline p-1"), "send-1")
	r := decodeRPC(t, res)
	if r.Error != nil {
		t.Fatalf("unexpected error: %+v", r.Error)
	}
	task := r.task(t)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("state = %q, want completed", task.Status.State)
	}
	// The streamed answer folded into the response artifact.
	if got := artifactText(task); !strings.Contains(got, "CONSUMER_LAG") {
		t.Errorf("artifact text = %q, want it to contain CONSUMER_LAG", got)
	}
	// And into the final status message.
	if task.Status.Message == nil || !strings.Contains(partsText(task.Status.Message.Parts), "CONSUMER_LAG") {
		t.Errorf("status message = %+v", task.Status.Message)
	}
}

func TestSendMessage_MissingProjectNameIsInvalidParams(t *testing.T) {
	srv := newTestServer(t)
	res := rpc(t, srv, goodToken, "SendMessage", sendMessageParams("", "hi"), "x")
	r := decodeRPC(t, res)
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("want -32602 invalid params, got %+v", r.Error)
	}
}

// ── GetTask ───────────────────────────────────────────────────

func TestGetTask(t *testing.T) {
	srv := newTestServer(t)
	sent := decodeRPC(t, rpc(t, srv, goodToken, "SendMessage", sendMessageParams(project, "hi"), "s"))
	taskID := sent.task(t).ID

	got := decodeRPC(t, rpc(t, srv, goodToken, "GetTask", map[string]any{"id": string(taskID)}, "g"))
	if got.Error != nil {
		t.Fatalf("GetTask error: %+v", got.Error)
	}
	if got.task(t).ID != taskID {
		t.Errorf("task id mismatch")
	}
}

func TestGetTask_403ForUngrantedProject(t *testing.T) {
	srv := newTestServer(t)
	sent := decodeRPC(t, rpc(t, srv, goodToken, "SendMessage", sendMessageParams(project, "hi"), "s"))
	taskID := sent.task(t).ID

	res := rpc(t, srv, wrongToken, "GetTask", map[string]any{"id": string(taskID)}, "g")
	defer res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
}

// ── CancelTask ────────────────────────────────────────────────

func TestCancelTask(t *testing.T) {
	srv := newTestServer(t)

	t.Run("terminal task not cancelable", func(t *testing.T) {
		sent := decodeRPC(t, rpc(t, srv, goodToken, "SendMessage", sendMessageParams(project, "hi"), "s"))
		taskID := sent.task(t).ID
		r := decodeRPC(t, rpc(t, srv, goodToken, "CancelTask", map[string]any{"id": string(taskID)}, "c"))
		if r.Error == nil || r.Error.Code != -32002 {
			t.Fatalf("want -32002 not-cancelable, got %+v", r.Error)
		}
	})

	t.Run("unknown task not found", func(t *testing.T) {
		r := decodeRPC(t, rpc(t, srv, goodToken, "CancelTask", map[string]any{"id": "does-not-exist"}, "c"))
		if r.Error == nil || r.Error.Code != -32001 {
			t.Fatalf("want -32001 not-found, got %+v", r.Error)
		}
	})
}

// ── JSON-RPC framing ──────────────────────────────────────────

func TestUnknownMethod(t *testing.T) {
	srv := newTestServer(t)
	r := decodeRPC(t, rpc(t, srv, goodToken, "does/notexist", map[string]any{}, "x"))
	if r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("want -32601 method-not-found, got %+v", r.Error)
	}
}

// ── SendStreamingMessage (SSE, v1.0 shape) ────────────────────

func TestSendStreamingMessage(t *testing.T) {
	srv := newTestServer(t)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "stream-1", "method": "SendStreamingMessage",
		"params": sendMessageParams(project, "Diagnose pipeline p-1"),
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/a2a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+goodToken)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	events := readSSE(t, res.Body)
	if len(events) == 0 {
		t.Fatal("no SSE events")
	}

	// First event is the submitted Task.
	if _, ok := events[0].Event.(*a2a.Task); !ok {
		t.Errorf("first event = %T, want *a2a.Task", events[0].Event)
	}

	var states []a2a.TaskState
	var artifactText strings.Builder
	var terminalState a2a.TaskState
	for _, e := range events {
		switch ev := e.Event.(type) {
		case *a2a.TaskStatusUpdateEvent:
			states = append(states, ev.Status.State)
			if ev.Status.State.Terminal() {
				terminalState = ev.Status.State
			}
		case *a2a.TaskArtifactUpdateEvent:
			artifactText.WriteString(partsText(ev.Artifact.Parts))
		}
	}
	if !containsState(states, a2a.TaskStateWorking) {
		t.Errorf("states %v missing working", states)
	}
	if terminalState != a2a.TaskStateCompleted {
		t.Errorf("terminal state = %q, want completed", terminalState)
	}
	if !strings.Contains(artifactText.String(), "CONSUMER_LAG") {
		t.Errorf("streamed artifact text = %q, want CONSUMER_LAG", artifactText.String())
	}
	// v1.0 wire uses TASK_STATE_* enums, not lowercase states.
	if !strings.HasPrefix(string(terminalState), "TASK_STATE_") {
		t.Errorf("state %q is not a v1.0 TASK_STATE_* enum", terminalState)
	}
}

// readSSE parses `data:` frames, each carrying a JSON-RPC response whose result
// is a StreamResponse (the A2A v1.0 oneOf wrapper).
func readSSE(t *testing.T, r io.Reader) []a2a.StreamResponse {
	t.Helper()
	var out []a2a.StreamResponse
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &resp); err != nil {
			t.Fatalf("bad SSE frame %q: %v", data, err)
		}
		if resp.Error != nil {
			t.Fatalf("SSE error frame: %+v", resp.Error)
		}
		if len(resp.Result) > 0 {
			var sr a2a.StreamResponse
			if err := json.Unmarshal(resp.Result, &sr); err != nil {
				t.Fatalf("decode SSE StreamResponse %q: %v", resp.Result, err)
			}
			out = append(out, sr)
		}
	}
	return out
}

// ── small helpers ─────────────────────────────────────────────

func artifactText(task *a2a.Task) string {
	var b strings.Builder
	for _, a := range task.Artifacts {
		b.WriteString(partsText(a.Parts))
	}
	return b.String()
}

func partsText(parts a2a.ContentParts) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text())
	}
	return b.String()
}

func containsState(states []a2a.TaskState, want a2a.TaskState) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}
