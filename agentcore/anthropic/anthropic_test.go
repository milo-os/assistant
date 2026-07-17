package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
