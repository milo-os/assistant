package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/logger"
)

// mentionRecordingRunner records the last request (and the ctx it arrived on)
// it was asked to run, and answers with a fixed, empty-of-events completion —
// enough to verify what chatStreamHandler builds from the request without
// exercising fakeRunner's streaming behavior too.
type mentionRecordingRunner struct {
	lastReq assistanta2a.RunRequest
	lastCtx context.Context
}

func (m *mentionRecordingRunner) Run(ctx context.Context, req assistanta2a.RunRequest, _ assistanta2a.RunSink) assistanta2a.RunResult {
	m.lastReq = req
	m.lastCtx = ctx
	return assistanta2a.RunResult{State: assistanta2a.RunCompleted, Text: "ok"}
}

func postChatStream(t *testing.T, srv *httptest.Server, token, contextID, projectID string, body map[string]any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	u := srv.URL + "/chat/conversations/" + contextID + "/sendmessage"
	if projectID != "" {
		u += "?" + url.Values{"projectId": {projectID}}.Encode()
	}
	req, _ := http.NewRequest(http.MethodPost, u, bytes.NewReader(raw))
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

// chatFrame is one decoded SSE data line from the flat vocabulary.
type chatFrame struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	State     string `json:"state"`
	Error     string `json:"error"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Summary   string `json:"summary"`
	OK        bool   `json:"ok"`
	ElapsedMs int64  `json:"elapsedMs"`
}

// readChatFrames decodes every `data: {...}` line off an SSE response body.
func readChatFrames(t *testing.T, res *http.Response) []chatFrame {
	t.Helper()
	defer res.Body.Close()
	var frames []chatFrame
	scanner := bufio.NewScanner(res.Body)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var f chatFrame
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			t.Fatalf("decode frame %q: %v", payload, err)
		}
		frames = append(frames, f)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE body: %v", err)
	}
	return frames
}

func TestChatStream_401WithoutToken(t *testing.T) {
	srv := newTestServer(t)
	res := postChatStream(t, srv, "", "c1", project, map[string]any{"text": "hi"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

func TestChatStream_403UngrantedProject(t *testing.T) {
	srv := newTestServer(t)
	res := postChatStream(t, srv, wrongToken, "c1", project, map[string]any{"text": "hi"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
}

func TestChatStream_400MissingProjectID(t *testing.T) {
	srv := newTestServer(t)
	res := postChatStream(t, srv, goodToken, "c1", "", map[string]any{"text": "hi"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestChatStream_400MissingText(t *testing.T) {
	srv := newTestServer(t)
	res := postChatStream(t, srv, goodToken, "c1", project, map[string]any{"text": ""})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// Authorization is checked against the query-string projectId, not anything
// in the body — there is no projectName in this request shape (unlike
// /v1alpha1/compact and /v1alpha1/conversations/rename), so a caller cannot
// smuggle a different project past authz by putting one in the body.
func TestChatStream_AuthorizesQueryProjectNotBody(t *testing.T) {
	srv := newTestServer(t)
	res := postChatStream(t, srv, wrongToken, "c1", "other-project", map[string]any{"text": "hi"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bob is granted other-project)", res.StatusCode)
	}
}

func TestChatStream_503WithoutRunner(t *testing.T) {
	cfg := testConfig(t)
	authn, authz := testAuth()
	app := New(Deps{
		Config:        cfg,
		Logger:        logger.Silent(),
		Authenticator: authn,
		Authorizer:    authz,
		Runner:        nil,
	})
	srv := httptest.NewServer(app)
	defer srv.Close()

	res := postChatStream(t, srv, goodToken, "c1", project, map[string]any{"text": "hi"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}

func TestChatStream_Success(t *testing.T) {
	srv := newTestServer(t)
	res := postChatStream(t, srv, goodToken, "c1", project, map[string]any{"text": "hello there"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	frames := readChatFrames(t, res)
	if len(frames) == 0 {
		t.Fatal("no frames decoded")
	}

	var text strings.Builder
	for _, f := range frames[:len(frames)-1] {
		if f.Type != "text_delta" {
			t.Fatalf("frame = %+v, want text_delta before the terminal frame", f)
		}
		text.WriteString(f.Text)
	}
	const want = "Patch here — how can I help with this project?"
	if got := text.String(); got != want {
		t.Fatalf("accumulated text = %q, want %q", got, want)
	}

	last := frames[len(frames)-1]
	if last.Type != "done" || last.State != "completed" || last.Text != want {
		t.Fatalf("terminal frame = %+v", last)
	}
}

// The diagnose branch of fakeRunner reports a tool call — verifying it reaches
// this endpoint as tool_start/tool_finish frames exercises the same sink path
// POST /a2a's activity status-updates take, just translated differently.
func TestChatStream_ToolActivity(t *testing.T) {
	srv := newTestServer(t)
	res := postChatStream(t, srv, goodToken, "c1", project, map[string]any{"text": "please diagnose pipeline p-1"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	frames := readChatFrames(t, res)

	var sawStart, sawFinish bool
	for _, f := range frames {
		switch f.Type {
		case "tool_start":
			sawStart = true
			if f.Name != diagnoseTool {
				t.Errorf("tool_start.name = %q, want %q", f.Name, diagnoseTool)
			}
		case "tool_finish":
			sawFinish = true
			if !f.OK {
				t.Errorf("tool_finish.ok = false, want true")
			}
			if f.ElapsedMs != 1200 {
				t.Errorf("tool_finish.elapsedMs = %d, want 1200", f.ElapsedMs)
			}
		}
	}
	if !sawStart || !sawFinish {
		t.Fatalf("frames = %+v, want a tool_start and a tool_finish", frames)
	}

	last := frames[len(frames)-1]
	if last.Type != "done" || last.State != "completed" {
		t.Fatalf("terminal frame = %+v", last)
	}
	if !strings.Contains(last.Text, "CONSUMER_LAG") {
		t.Fatalf("terminal frame text = %q, want it to contain the findings", last.Text)
	}
}

func TestChatStream_MentionsReachTheRunner(t *testing.T) {
	recorder := &mentionRecordingRunner{}
	cfg := testConfig(t)
	authn, authz := testAuth()
	app := New(Deps{
		Config:        cfg,
		Logger:        logger.Silent(),
		Authenticator: authn,
		Authorizer:    authz,
		Runner:        recorder,
	})
	srv := httptest.NewServer(app)
	defer srv.Close()

	res := postChatStream(t, srv, goodToken, "c1", project, map[string]any{
		"text":     "hi",
		"mentions": []map[string]any{{"kind": "Workload", "name": "web", "apiGroup": "compute.datumapis.com"}},
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	_ = readChatFrames(t, res)

	if len(recorder.lastReq.Mentions) != 1 {
		t.Fatalf("mentions = %+v, want exactly one", recorder.lastReq.Mentions)
	}
	got := recorder.lastReq.Mentions[0]
	if got.Kind != "Workload" || got.Name != "web" || got.APIGroup != "compute.datumapis.com" {
		t.Fatalf("mention = %+v", got)
	}
	if recorder.lastReq.ContextID != "c1" {
		t.Fatalf("contextID = %q, want c1", recorder.lastReq.ContextID)
	}
	if recorder.lastReq.ProjectName != project {
		t.Fatalf("projectName = %q, want %q", recorder.lastReq.ProjectName, project)
	}
}

// This is the regression this endpoint exists to fix, restated as a test: a
// capability provider (e.g. compute's MCP tool) only gets a forwardable
// identity when the caller's raw bearer token is on the context Run receives,
// via [auth.BearerTokenFromContext]. authMiddleware stamps this for POST /a2a;
// chatStreamHandler must stamp it too, since it authenticates independently
// rather than going through that middleware. Caught live in staging: a
// request through this exact endpoint completed but the tool call inside it
// reported no identity was forwarded, because this line was missing.
func TestChatStream_ForwardsBearerTokenForCapabilityProviders(t *testing.T) {
	recorder := &mentionRecordingRunner{}
	cfg := testConfig(t)
	authn, authz := testAuth()
	app := New(Deps{
		Config:        cfg,
		Logger:        logger.Silent(),
		Authenticator: authn,
		Authorizer:    authz,
		Runner:        recorder,
	})
	srv := httptest.NewServer(app)
	defer srv.Close()

	res := postChatStream(t, srv, goodToken, "c1", project, map[string]any{"text": "hi"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	_ = readChatFrames(t, res)

	if recorder.lastCtx == nil {
		t.Fatal("runner never ran")
	}
	if got := auth.BearerTokenFromContext(recorder.lastCtx); got != goodToken {
		t.Fatalf("bearer token on ctx = %q, want %q (capability providers forward this)", got, goodToken)
	}
}
