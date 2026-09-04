package openaicompat

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

func chunk(fields map[string]any) string {
	base := map[string]any{"id": "cmpl-1", "object": "chat.completion.chunk", "created": 0, "model": "patch-stub-v1"}
	for k, v := range fields {
		base[k] = v
	}
	b, _ := json.Marshal(base)
	return "data: " + string(b) + "\n\n"
}

// textSSE streams a short answer and a final usage-only chunk that reports
// cached prompt tokens.
func textSSE() string {
	return strings.Join([]string{
		chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}}),
		chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "All clear — provider says OK for p-1."}, "finish_reason": nil}}}),
		chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}),
		chunk(map[string]any{"choices": []any{}, "usage": map[string]any{
			"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18,
			"prompt_tokens_details": map[string]any{"cached_tokens": 4},
		}}),
		"data: [DONE]\n\n",
	}, "")
}

func toolSSE() string {
	return strings.Join([]string{
		chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}}),
		chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "streamco__pipeline_diagnose", "arguments": `{"id":"p-1"}`}}},
		}, "finish_reason": nil}}}),
		chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}}),
		chunk(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 9, "total_tokens": 29}}),
		"data: [DONE]\n\n",
	}, "")
}

type capture struct {
	headers http.Header
	body    map[string]any
}

func server(t *testing.T, sse string, cap *capture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.headers = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &cap.body)
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

func stream(t *testing.T, m *Model, req agentcore.Request) agentcore.StreamReader {
	t.Helper()
	s, err := m.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return s
}

func TestStreamTextAndUsageWithCache(t *testing.T) {
	var cap capture
	srv := server(t, textSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "patch-stub-v1", BaseURL: srv.URL})
	parts := drain(t, stream(t, m, agentcore.Request{
		System:          "sys",
		Messages:        []agentcore.Message{agentcore.UserMessage("Diagnose pipeline p-1")},
		MaxOutputTokens: 4096,
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
	if !strings.Contains(text.String(), "provider says OK") {
		t.Fatalf("text = %q", text.String())
	}
	if finish == nil {
		t.Fatal("no step-finish")
	}
	want := agentcore.Usage{Input: 11, Output: 7, CacheRead: 4}
	if finish.Usage != want {
		t.Fatalf("usage = %+v, want %+v", finish.Usage, want)
	}
	if finish.FinishReason != agentcore.FinishStop {
		t.Fatalf("reason = %v", finish.FinishReason)
	}
	// The request declared usage reporting and carried the model id.
	if cap.body["model"] != "patch-stub-v1" {
		t.Fatalf("model on wire = %v", cap.body["model"])
	}
	if so, ok := cap.body["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage not set: %v", cap.body["stream_options"])
	}
}

func TestStreamGatewayModeAttributionAndNoAuthorization(t *testing.T) {
	var cap capture
	srv := server(t, textSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "patch-stub-v1", BaseURL: srv.URL}) // no API key
	drain(t, stream(t, m, agentcore.Request{
		Messages: []agentcore.Message{agentcore.UserMessage("hello")},
		Headers: map[string]string{
			"x-datum-project":      "demo-project",
			"x-datum-conversation": "conv-9",
			"x-datum-agent":        "patch",
		},
	}))

	if got := cap.headers.Get("x-datum-project"); got != "demo-project" {
		t.Fatalf("x-datum-project=%q", got)
	}
	if got := cap.headers.Get("x-datum-conversation"); got != "conv-9" {
		t.Fatalf("x-datum-conversation=%q", got)
	}
	if got := cap.headers.Get("x-datum-agent"); got != "patch" {
		t.Fatalf("x-datum-agent=%q", got)
	}
	if got := cap.headers.Get("Authorization"); got != "" {
		t.Fatalf("gateway mode must send NO Authorization header, got %q", got)
	}
}

func TestStreamWithAPIKeySendsBearer(t *testing.T) {
	var cap capture
	srv := server(t, textSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "patch-stub-v1", BaseURL: srv.URL, APIKey: "sk-test"})
	drain(t, stream(t, m, agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("hi")}}))

	if got := cap.headers.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want Bearer sk-test", got)
	}
}

func TestStreamEmitsToolCall(t *testing.T) {
	var cap capture
	srv := server(t, toolSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "patch-stub-v1", BaseURL: srv.URL})
	parts := drain(t, stream(t, m, agentcore.Request{
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
	if call == nil || call.ID != "call_1" || call.Name != "streamco__pipeline_diagnose" {
		t.Fatalf("tool call = %+v", call)
	}
	if string(call.Input) != `{"id":"p-1"}` {
		t.Fatalf("tool input = %s", call.Input)
	}
	if finish == nil || finish.FinishReason != agentcore.FinishToolCalls {
		t.Fatalf("finish = %+v", finish)
	}
	tools, _ := cap.body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool on the wire, got %d", len(tools))
	}
}

// TestMisroutedRequestFailsLoudly pins the fix for a silent-failure mode
// found live: a gateway 404 (text/plain "unsupported path") gave the SSE
// parser nothing to parse, which surfaced as a COMPLETED, empty, zero-token
// turn. Any response with no stream events must be an error part.
func TestMisroutedRequestFailsLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("unsupported path: " + r.URL.Path))
	}))
	defer srv.Close()

	m := New(Options{BaseURL: srv.URL, ModelID: "patch-stub-v1"})
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

// errorServer returns an HTTP error with the given status/body/headers on every
// request, counting hits so a test can prove the adapter makes exactly one
// request (SDK-internal retry must be off; retry is the loop's job).
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

// TestClassifiesStatuses pins that OpenAI-compatible HTTP errors surface as
// classified [agentcore.ModelError]s: 429/503 retryable, 401/400 terminal, and
// that the adapter makes exactly one request (no hidden SDK retry).
func TestClassifiesStatuses(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantClass agentcore.ErrorClass
		retryable bool
	}{
		{"rate-limited", 429, `{"error":{"message":"rate limit","type":"rate_limit_exceeded"}}`, agentcore.ErrClassRateLimited, true},
		{"unavailable", 503, `{"error":{"message":"unavailable"}}`, agentcore.ErrClassOverloaded, true},
		{"auth", 401, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`, agentcore.ErrClassAuth, false},
		{"invalid", 400, `{"error":{"message":"bad param","type":"invalid_request_error"}}`, agentcore.ErrClassInvalidRequest, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits int
			srv := errorServer(t, c.status, c.body, nil, &hits)
			defer srv.Close()

			m := New(Options{ModelID: "patch-stub-v1", BaseURL: srv.URL})
			err := streamErr(t, m)
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

// TestClassifiesContextLength pins that a 400 the endpoint attributes to
// context length is bucketed as terminal context-length with a clear message.
func TestClassifiesContextLength(t *testing.T) {
	var hits int
	srv := errorServer(t, 400, `{"error":{"message":"This model's maximum context length is 128000 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`, nil, &hits)
	defer srv.Close()

	m := New(Options{ModelID: "patch-stub-v1", BaseURL: srv.URL})
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

// TestClassifyHonorsRetryAfterHeader pins that a Retry-After on a 429 lands on
// the classified error for the loop to honor.
func TestClassifyHonorsRetryAfterHeader(t *testing.T) {
	var hits int
	srv := errorServer(t, 429, `{"error":{"message":"slow"}}`, map[string]string{"Retry-After": "3"}, &hits)
	defer srv.Close()

	m := New(Options{ModelID: "patch-stub-v1", BaseURL: srv.URL})
	err := streamErr(t, m)
	var me *agentcore.ModelError
	if !errors.As(err, &me) {
		t.Fatalf("not a ModelError: %v", err)
	}
	if me.RetryAfter != 3*time.Second {
		t.Fatalf("RetryAfter = %v, want 3s", me.RetryAfter)
	}
}

// TestClassifyPassesThroughCancellation pins that a cancellation/deadline is
// returned unchanged (never a retryable ModelError), so the loop ends canceled.
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
	if _, isModelErr := classify(fmt.Errorf("do request: %w", context.Canceled)).(*agentcore.ModelError); isModelErr {
		t.Fatal("a wrapped cancellation must not be classified as a retryable ModelError")
	}
}

// The gateway token is a projected ServiceAccount token: the kubelet rewrites
// it in place long before expiry. Reading it once at construction would
// authenticate until the first rotation and then 401 forever, with nothing in
// this process changing to explain it — so assert the SECOND request picks up a
// token written after the client was built.
func TestStreamGatewayModeTokenFileRereadPerRequest(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("first-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	var cap capture
	srv := server(t, textSSE(), &cap)
	defer srv.Close()

	m := New(Options{ModelID: "patch-stub-v1", BaseURL: srv.URL, TokenFile: tokenPath})

	drain(t, stream(t, m, agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("hello")}}))
	if got := cap.headers.Get("Authorization"); got != "Bearer first-token" {
		t.Fatalf("first request Authorization=%q, want %q", got, "Bearer first-token")
	}

	// Rotate in place, exactly as the kubelet does.
	if err := os.WriteFile(tokenPath, []byte("second-token\n"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}

	drain(t, stream(t, m, agentcore.Request{Messages: []agentcore.Message{agentcore.UserMessage("again")}}))
	if got := cap.headers.Get("Authorization"); got != "Bearer second-token" {
		t.Fatalf("after rotation Authorization=%q, want %q — a token read once at startup would still send the first",
			got, "Bearer second-token")
	}
}

// An unreadable token must surface as a missing token here, not as an
// unexplained 401 from the gateway. This adapter reports failures as a terminal
// error part rather than a Recv error, so assert on that.
func TestStreamGatewayModeTokenFileUnreadable(t *testing.T) {
	var cap capture
	srv := server(t, textSSE(), &cap)
	defer srv.Close()

	missing := filepath.Join(t.TempDir(), "absent")
	m := New(Options{ModelID: "patch-stub-v1", BaseURL: srv.URL, TokenFile: missing})

	parts := drain(t, stream(t, m, agentcore.Request{
		Messages: []agentcore.Message{agentcore.UserMessage("hello")},
	}))
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
