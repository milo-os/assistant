package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/milo-os/assistant/agentcore"
)

// sseFrame renders one Anthropic SSE event.
func sseFrame(event string, data any) string {
	b, _ := json.Marshal(data)
	return "event: " + event + "\ndata: " + string(b) + "\n\n"
}

// textStreamSSE is a full streamed text answer whose usage reports cache
// read and cache write tokens (message_start carries input+cache; the
// message_delta carries the output count).
func textStreamSSE() string {
	return strings.Join([]string{
		sseFrame("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-test",
				"content": []any{}, "stop_reason": nil,
				"usage": map[string]any{
					"input_tokens": 80, "cache_read_input_tokens": 20,
					"cache_creation_input_tokens": 15, "output_tokens": 1,
				},
			},
		}),
		sseFrame("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		}),
		sseFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "All clear."},
		}),
		sseFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sseFrame("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 30},
		}),
		sseFrame("message_stop", map[string]any{"type": "message_stop"}),
	}, "")
}

// toolStreamSSE is a streamed turn that requests a tool call and stops with
// tool_use.
func toolStreamSSE() string {
	return strings.Join([]string{
		sseFrame("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_2", "type": "message", "role": "assistant", "model": "claude-test",
				"content": []any{}, "stop_reason": nil,
				"usage": map[string]any{"input_tokens": 40, "output_tokens": 1},
			},
		}),
		sseFrame("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "streamco__pipeline_diagnose", "input": map[string]any{}},
		}),
		sseFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"id":"p-1"}`},
		}),
		sseFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sseFrame("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 12},
		}),
		sseFrame("message_stop", map[string]any{"type": "message_stop"}),
	}, "")
}

type capturedRequest struct {
	headers http.Header
	body    map[string]any
}

func fakeServer(t *testing.T, sse string, captured *capturedRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.headers = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
	}))
}

func drain(t *testing.T, s agentcore.StreamReader) []agentcore.StreamPart {
	t.Helper()
	defer s.Close()
	var parts []agentcore.StreamPart
	for {
		p, err := s.Recv()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		parts = append(parts, p)
	}
}

func TestStreamExtractsCacheReadAndWriteUsage(t *testing.T) {
	var cap capturedRequest
	srv := fakeServer(t, textStreamSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
	parts := drain(t, mustStream(t, m, agentcore.Request{
		System:   "sys",
		Messages: []agentcore.Message{agentcore.UserMessage("hi")},
	}))

	var text strings.Builder
	var finish *agentcore.StreamPart
	for i := range parts {
		switch parts[i].Kind {
		case agentcore.StreamPartTextDelta:
			text.WriteString(parts[i].Text)
		case agentcore.StreamPartStepFinish:
			finish = &parts[i]
		}
	}
	if text.String() != "All clear." {
		t.Fatalf("text = %q", text.String())
	}
	if finish == nil {
		t.Fatal("no step-finish emitted")
	}
	want := agentcore.Usage{Input: 100, Output: 30, CacheRead: 20, CacheWrite: 15}
	if finish.Usage != want {
		t.Fatalf("usage = %+v, want %+v (Input must include cache read)", finish.Usage, want)
	}
	if finish.FinishReason != agentcore.FinishStop {
		t.Fatalf("finish reason = %v", finish.FinishReason)
	}
}

func TestStreamGatewayModeSendsAttributionAndNoCredential(t *testing.T) {
	var cap capturedRequest
	srv := fakeServer(t, textStreamSSE(), &cap)
	defer srv.Close()

	// Gateway posture: no API key, base URL points at the gateway,
	// attribution headers supplied per-request.
	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
	drain(t, mustStream(t, m, agentcore.Request{
		Messages: []agentcore.Message{agentcore.UserMessage("hi")},
		Headers: map[string]string{
			"x-datum-project":      "demo-project",
			"x-datum-conversation": "conv-1",
			"x-datum-agent":        "patch",
		},
	}))

	if got := cap.headers.Get("x-datum-project"); got != "demo-project" {
		t.Fatalf("attribution header not forwarded: x-datum-project=%q", got)
	}
	if got := cap.headers.Get("x-datum-agent"); got != "patch" {
		t.Fatalf("x-datum-agent=%q", got)
	}
	if got := cap.headers.Get("x-api-key"); got != "" {
		t.Fatalf("no credential must be sent in gateway mode, got x-api-key=%q", got)
	}
	if got := cap.headers.Get("Authorization"); got != "" {
		t.Fatalf("no Authorization must be sent in gateway mode, got %q", got)
	}
}

func TestStreamWithAPIKeySendsCredential(t *testing.T) {
	var cap capturedRequest
	srv := fakeServer(t, textStreamSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL, APIKey: "sk-secret"})
	drain(t, mustStream(t, m, agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("hi")}}))

	if got := cap.headers.Get("x-api-key"); got != "sk-secret" {
		t.Fatalf("x-api-key = %q, want sk-secret", got)
	}
}

func TestStreamEmitsToolCall(t *testing.T) {
	var cap capturedRequest
	srv := fakeServer(t, toolStreamSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
	parts := drain(t, mustStream(t, m, agentcore.Request{
		Messages: []agentcore.Message{agentcore.UserMessage("diagnose p-1")},
		Tools: []agentcore.ToolDefinition{{
			Name:        "streamco__pipeline_diagnose",
			Description: "diagnose",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		}},
	}))

	var call *agentcore.ToolCall
	var finish *agentcore.StreamPart
	for i := range parts {
		switch parts[i].Kind {
		case agentcore.StreamPartToolCall:
			call = parts[i].ToolCall
		case agentcore.StreamPartStepFinish:
			finish = &parts[i]
		}
	}
	if call == nil {
		t.Fatal("no tool call emitted")
	}
	if call.ID != "toolu_1" || call.Name != "streamco__pipeline_diagnose" {
		t.Fatalf("tool call = %+v", call)
	}
	if string(call.Input) != `{"id":"p-1"}` {
		t.Fatalf("tool input = %s", call.Input)
	}
	if finish == nil || finish.FinishReason != agentcore.FinishToolCalls {
		t.Fatalf("finish = %+v", finish)
	}

	// The request body carried the tool definition to the wire.
	tools, _ := cap.body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool on the wire, got %d", len(tools))
	}
}

// TestMisroutedRequestFailsLoudly pins the fix for a silent-failure mode: a
// proxy that returns HTTP 200 with a non-SSE body (HTML, or a JSON object
// because it ignored stream:true) gives the SSE parser nothing to parse,
// which surfaced as a COMPLETED, empty, zero-token turn. Any response with no
// stream events must be an error part.
func TestMisroutedRequestFailsLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[]}`)
	}))
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
	s, err := m.Stream(context.Background(), agentcore.Request{
		Messages: []agentcore.Message{agentcore.UserMessage("hi")},
	})
	if err != nil {
		return // an eager error is also acceptable
	}
	defer s.Close()

	var sawError bool
	for {
		p, rerr := s.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			sawError = true
			break
		}
		if p.Kind == agentcore.StreamPartError {
			sawError = true
			break
		}
		if p.Kind == agentcore.StreamPartStepFinish {
			t.Fatalf("misrouted request finished gracefully (usage %+v) — must error", p.Usage)
		}
	}
	if !sawError {
		t.Fatal("misrouted request produced neither an error part nor a stream error")
	}
}

func mustStream(t *testing.T, m *Model, req agentcore.Request) agentcore.StreamReader {
	t.Helper()
	s, err := m.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return s
}

// errorServer returns an HTTP error with the given status/body/headers on every
// request, and records how many requests it saw (to prove the adapter does not
// retry internally — retry is the loop's job).
func errorServer(t *testing.T, status int, body string, headers map[string]string, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

// streamErr drives the model and returns the terminal error the stream carries.
func streamErr(t *testing.T, m *Model) error {
	t.Helper()
	s, err := m.Stream(context.Background(), agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("hi")}})
	if err != nil {
		return err
	}
	defer s.Close()
	for {
		p, rerr := s.Recv()
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		if p.Kind == agentcore.StreamPartError {
			return p.Err
		}
	}
}

// TestClassifiesRetryableStatuses pins that HTTP errors surface as classified
// [agentcore.ModelError]s the loop can act on: 429/503/529 retryable, 400/401
// terminal — and that the adapter itself makes exactly one request (no hidden
// SDK retry compounding the loop's).
func TestClassifiesRetryableStatuses(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantClass agentcore.ErrorClass
		retryable bool
	}{
		{"rate-limited", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, agentcore.ErrClassRateLimited, true},
		{"overloaded", 529, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`, agentcore.ErrClassOverloaded, true},
		{"unavailable", 503, `{"type":"error","error":{"type":"api_error","message":"unavailable"}}`, agentcore.ErrClassOverloaded, true},
		{"auth", 401, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, agentcore.ErrClassAuth, false},
		{"invalid", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`, agentcore.ErrClassInvalidRequest, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits int
			srv := errorServer(t, c.status, c.body, nil, &hits)
			defer srv.Close()

			m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
			err := streamErr(t, m)
			if err == nil {
				t.Fatal("expected an error part")
			}
			var me *agentcore.ModelError
			if !errors.As(err, &me) {
				t.Fatalf("error is not a classified ModelError: %v", err)
			}
			if me.Class != c.wantClass {
				t.Fatalf("class = %v, want %v", me.Class, c.wantClass)
			}
			if me.Class.Retryable() != c.retryable {
				t.Fatalf("retryable = %v, want %v", me.Class.Retryable(), c.retryable)
			}
			if hits != 1 {
				t.Fatalf("adapter made %d requests, want exactly 1 (SDK retry must be off)", hits)
			}
		})
	}
}

// TestClassifiesContextLength pins that a length-flavored 400 is bucketed as
// context-length (terminal) and surfaces a clear, actionable message — a real
// model hits this on long histories.
func TestClassifiesContextLength(t *testing.T) {
	var hits int
	srv := errorServer(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`, nil, &hits)
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
	err := streamErr(t, m)
	var me *agentcore.ModelError
	if !errors.As(err, &me) {
		t.Fatalf("not a ModelError: %v", err)
	}
	if me.Class != agentcore.ErrClassContextLength {
		t.Fatalf("class = %v, want context-length", me.Class)
	}
	if me.Class.Retryable() {
		t.Fatal("context-length must be terminal")
	}
	if !strings.Contains(err.Error(), "context window") {
		t.Fatalf("message should be actionable, got %q", err.Error())
	}
}

// TestClassifyHonorsRetryAfterHeader pins that a Retry-After on a 429 is parsed
// onto the classified error so the loop can honor the server's pacing.
func TestClassifyHonorsRetryAfterHeader(t *testing.T) {
	var hits int
	srv := errorServer(t, 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`,
		map[string]string{"Retry-After": "7"}, &hits)
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
	err := streamErr(t, m)
	var me *agentcore.ModelError
	if !errors.As(err, &me) {
		t.Fatalf("not a ModelError: %v", err)
	}
	if me.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", me.RetryAfter)
	}
}

// TestClassifyPassesThroughCancellation pins that a context cancellation or
// deadline expiry is NOT wrapped into a retryable [agentcore.ModelError]: it is
// returned unchanged so the loop ends the run as canceled rather than retrying
// a dead request. (classify is exercised directly — the concrete cancellation
// error the SDK surfaces on an aborted request is context.Canceled/Deadline.)
func TestClassifyPassesThroughCancellation(t *testing.T) {
	for _, cancelErr := range []error{context.Canceled, context.DeadlineExceeded} {
		got := classify(cancelErr)
		if !errors.Is(got, cancelErr) {
			t.Fatalf("classify(%v) = %v, want the cancellation passed through", cancelErr, got)
		}
		var me *agentcore.ModelError
		if errors.As(got, &me) {
			t.Fatalf("cancellation must not become a ModelError, got class %v", me.Class)
		}
	}
	// A wrapped cancellation is also recognized (SDKs wrap the context error).
	if _, isModelErr := classify(fmt.Errorf("do request: %w", context.Canceled)).(*agentcore.ModelError); isModelErr {
		t.Fatal("a wrapped cancellation must not be classified as a retryable ModelError")
	}
}

// The gateway token is a projected ServiceAccount token the kubelet rewrites in
// place, so the second request must carry a token written after construction.
func TestStreamGatewayModeTokenFileRereadPerRequest(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("first-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	var cap capturedRequest
	srv := fakeServer(t, textStreamSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL, TokenFile: tokenPath})

	drain(t, mustStream(t, m, agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("hello")}}))
	if got := cap.headers.Get("Authorization"); got != "Bearer first-token" {
		t.Fatalf("first request Authorization=%q, want %q", got, "Bearer first-token")
	}
	if got := cap.headers.Get("x-api-key"); got != "" {
		t.Fatalf("a gateway token is not a model credential, got x-api-key=%q", got)
	}

	if err := os.WriteFile(tokenPath, []byte("second-token\n"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}

	drain(t, mustStream(t, m, agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("again")}}))
	if got := cap.headers.Get("Authorization"); got != "Bearer second-token" {
		t.Fatalf("after rotation Authorization=%q, want %q", got, "Bearer second-token")
	}
}

func TestStreamGatewayModeTokenFileUnreadable(t *testing.T) {
	var cap capturedRequest
	srv := fakeServer(t, textStreamSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL, TokenFile: filepath.Join(t.TempDir(), "absent")})

	parts := drain(t, mustStream(t, m, agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("hello")}}))
	if len(parts) == 0 {
		t.Fatal("want a terminal error part, got no parts")
	}
	last := parts[len(parts)-1]
	if last.Kind != agentcore.StreamPartError || last.Err == nil {
		t.Fatalf("last part = %+v, want a StreamPartError carrying an error", last)
	}
	if !strings.Contains(last.Err.Error(), "reading gateway token") {
		t.Fatalf("err = %v, want it to name the unreadable token file", last.Err)
	}
	if cap.headers != nil {
		t.Error("no request should have been sent without a token")
	}
}

func cacheMarked(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	cc, ok := m["cache_control"].(map[string]any)
	return ok && cc["type"] == "ephemeral"
}

func lastBlock(t *testing.T, list any) any {
	t.Helper()
	items, ok := list.([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("want a non-empty list, got %#v", list)
	}
	return items[len(items)-1]
}

func countMarked(list any) int {
	items, _ := list.([]any)
	n := 0
	for _, item := range items {
		if cacheMarked(item) {
			n++
		}
	}
	return n
}

func TestStreamMarksCacheBreakpoints(t *testing.T) {
	var cap capturedRequest
	srv := fakeServer(t, textStreamSSE(), &cap)
	defer srv.Close()

	schema := json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`)
	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
	drain(t, mustStream(t, m, agentcore.Request{
		System: "sys",
		Tools: []agentcore.ToolDefinition{
			{Name: "first", InputSchema: schema},
			{Name: "second", InputSchema: schema},
		},
		Messages: []agentcore.Message{
			agentcore.UserMessage("diagnose p-1"),
			{Role: agentcore.RoleAssistant, Content: []agentcore.ContentPart{
				agentcore.TextPart("checking"),
				{Kind: agentcore.ContentToolCall, ToolCall: &agentcore.ToolCall{ID: "tc-1", Name: "first", Input: json.RawMessage(`{"id":"p-1"}`)}},
			}},
			{Role: agentcore.RoleTool, Content: []agentcore.ContentPart{
				{Kind: agentcore.ContentToolResult, ToolResult: &agentcore.ToolResult{ToolCallID: "tc-1", Name: "first", Content: "lagging"}},
			}},
		},
	}))

	tools := cap.body["tools"]
	if !cacheMarked(lastBlock(t, tools)) || countMarked(tools) != 1 {
		t.Errorf("want only the last tool marked, got %v", tools)
	}
	if !cacheMarked(lastBlock(t, cap.body["system"])) {
		t.Errorf("want the system prompt marked, got %v", cap.body["system"])
	}

	messages, _ := cap.body["messages"].([]any)
	for i, msg := range messages {
		content := msg.(map[string]any)["content"]
		want := 0
		if i == len(messages)-1 {
			want = 1
			if !cacheMarked(lastBlock(t, content)) {
				t.Errorf("want the final block of the last message marked, got %v", content)
			}
		}
		if got := countMarked(content); got != want {
			t.Errorf("message %d: %d marked blocks, want %d", i, got, want)
		}
	}
}

func TestStreamCachesWithoutToolsOrSystem(t *testing.T) {
	var cap capturedRequest
	srv := fakeServer(t, textStreamSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
	drain(t, mustStream(t, m, agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("hi")}}))

	if _, ok := cap.body["tools"]; ok {
		t.Errorf("no tools were given, got %v", cap.body["tools"])
	}
	if _, ok := cap.body["system"]; ok {
		t.Errorf("no system prompt was given, got %v", cap.body["system"])
	}
	messages, _ := cap.body["messages"].([]any)
	if !cacheMarked(lastBlock(t, lastBlock(t, messages).(map[string]any)["content"])) {
		t.Errorf("want the only message marked, got %v", messages)
	}
}

// The API refuses cache_control on an empty text block, so a marker there would
// turn a request about empty input into a confusing cache error.
func TestStreamSkipsCacheMarkOnEmptyText(t *testing.T) {
	var cap capturedRequest
	srv := fakeServer(t, textStreamSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "claude-test", BaseURL: srv.URL})
	drain(t, mustStream(t, m, agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("")}}))

	messages, _ := cap.body["messages"].([]any)
	if countMarked(lastBlock(t, messages).(map[string]any)["content"]) != 0 {
		t.Errorf("an empty text block must not be marked, got %v", messages)
	}
}
