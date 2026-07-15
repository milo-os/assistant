package openaicompat

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
