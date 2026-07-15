package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/agentcore/mockmodel"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/usage"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// noopEmitter is an emitter with no collector configured (Emit is a no-op).
func noopEmitter() *usage.Emitter { return usage.NewEmitter(usage.EmitterConfig{}) }

// fakeSource returns fixed documents for any project.
type fakeSource struct{ docs []capability.CapabilityDocument }

func (s fakeSource) Documents(context.Context, string) ([]capability.CapabilityDocument, error) {
	return s.docs, nil
}

// mcpServerWithDiagnose starts an in-process MCP server exposing
// pipeline_diagnose and returns its URL.
func mcpServerWithDiagnose(t *testing.T) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "streamco", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "pipeline_diagnose", Description: "Diagnose",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &args)
			out, _ := json.Marshal(map[string]any{"id": args.ID, "findings": []string{"CONSUMER_LAG"}})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out)}}}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func diagnoseDoc(endpoint string) capability.CapabilityDocument {
	return capability.CapabilityDocument{
		Spec: capability.CapabilitySpec{
			ServiceRef:           capability.Ref{Name: "streamco"},
			ServiceName:          "streaming.streamco.example",
			ServiceAgentRef:      capability.Ref{Name: "streamco-agent"},
			ConfigurationVersion: "v1",
			Tools: &capability.Tools{MCPServers: []capability.MCPServer{{
				Name:         "streamco",
				Endpoint:     endpoint,
				ToolSelector: capability.ToolSelector{Include: []string{"pipeline_diagnose"}},
			}}},
		},
	}
}

func drainEvents(t *testing.T, s *Stream) []Event {
	t.Helper()
	var events []Event
	for {
		e, err := s.Recv()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		events = append(events, e)
	}
}

// TestFullMockToolPath proves the whole chat path with no API key: the mock
// model requests a tool over a real MCP server, the result folds into a final
// answer, and usage (tokens + one tool invocation) is metered.
func TestFullMockToolPath(t *testing.T) {
	endpoint := mcpServerWithDiagnose(t)
	conv := New(Deps{
		Model:     mockmodel.New(),
		ModelMode: "mock",
		Source:    fakeSource{docs: []capability.CapabilityDocument{diagnoseDoc(endpoint)}},
		Emitter:   noopEmitter(),
	})

	stream := conv.Run(context.Background(), Params{
		UserText: "Please diagnose pipeline p-1", ProjectName: "demo-project", ContextID: "conv-1", TaskID: "task-1",
	})
	events := drainEvents(t, stream)
	result := stream.Result()

	if result.State != StateCompleted {
		t.Fatalf("state = %s (err=%s)", result.State, result.Error)
	}
	var sawToolCall bool
	for _, e := range events {
		if e.Kind == EventToolCall && e.ToolName == "streamco__pipeline_diagnose" {
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Fatal("expected a tool-call event for the namespaced tool")
	}
	if !strings.Contains(result.Text, "CONSUMER_LAG") {
		t.Fatalf("final answer should quote the tool findings, got: %q", result.Text)
	}

	// Usage: token meters + exactly one tool-invocation event.
	var toolEvents, inputEvents int
	for _, e := range result.UsageEvents {
		switch {
		case strings.HasSuffix(e.MeterName, "tool-invocations"):
			toolEvents++
			if e.Dimensions["service"] != "streaming.streamco.example" {
				t.Fatalf("tool event service dim = %q", e.Dimensions["service"])
			}
		case strings.HasSuffix(e.MeterName, "input-tokens"):
			inputEvents++
		}
	}
	if toolEvents != 1 {
		t.Fatalf("want 1 tool-invocation event, got %d", toolEvents)
	}
	if inputEvents == 0 {
		t.Fatal("expected input-token meter")
	}
	if result.Usage.ToolInvocationEventCount != 1 {
		t.Fatalf("ToolInvocationEventCount = %d", result.Usage.ToolInvocationEventCount)
	}
}

// cacheModel is a one-step model that reports cache read/write tokens.
type cacheModel struct{ headers map[string]string }

func (m *cacheModel) ModelID() string { return "cache-pin" }
func (m *cacheModel) Stream(_ context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	m.headers = req.Headers
	return &partReader{parts: []agentcore.StreamPart{
		{Kind: agentcore.StreamPartTextDelta, Text: "ok"},
		{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishStop,
			Usage: agentcore.Usage{Input: 100, Output: 30, CacheRead: 20, CacheWrite: 15}},
	}}, nil
}

type partReader struct {
	parts []agentcore.StreamPart
	i     int
}

func (r *partReader) Recv() (agentcore.StreamPart, error) {
	if r.i >= len(r.parts) {
		return agentcore.StreamPart{}, io.EOF
	}
	p := r.parts[r.i]
	r.i++
	return p, nil
}
func (r *partReader) Close() error { return nil }

// TestCacheTokenMetering pins that cache read/write survive into the meters
// (the TS bug class: dropping cache fields).
func TestCacheTokenMetering(t *testing.T) {
	conv := New(Deps{Model: &cacheModel{}, ModelMode: "mock", Emitter: noopEmitter()})
	stream := conv.Run(context.Background(), Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-cache"})
	drainEvents(t, stream)

	byMeter := map[string]string{}
	for _, e := range stream.Result().UsageEvents {
		byMeter[e.MeterName] = e.Value
	}
	cases := map[string]string{
		usage.MeterInputTokens:      "100",
		usage.MeterOutputTokens:     "30",
		usage.MeterCacheReadTokens:  "20",
		usage.MeterCacheWriteTokens: "15",
	}
	for meter, want := range cases {
		if byMeter[meter] != want {
			t.Fatalf("%s = %q, want %q", meter, byMeter[meter], want)
		}
	}
}

// TestGatewayModeSetsAttributionHeaders verifies the gateway attribution
// headers reach the model call only in gateway mode.
func TestGatewayModeSetsAttributionHeaders(t *testing.T) {
	gw := &cacheModel{}
	convGW := New(Deps{Model: gw, ModelMode: "gateway", Emitter: noopEmitter()})
	drainEvents(t, convGW.Run(context.Background(), Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-9"}))

	if gw.headers["x-datum-project"] != "demo-project" || gw.headers["x-datum-agent"] != "patch" || gw.headers["x-datum-conversation"] != "conv-9" {
		t.Fatalf("gateway attribution headers = %v", gw.headers)
	}

	mock := &cacheModel{}
	convMock := New(Deps{Model: mock, ModelMode: "mock", Emitter: noopEmitter()})
	drainEvents(t, convMock.Run(context.Background(), Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-9"}))
	if mock.headers != nil {
		t.Fatalf("non-gateway mode must send no attribution headers, got %v", mock.headers)
	}
}

// TestNoProjectSkipsMetering verifies nothing is billed without a project.
func TestNoProjectSkipsMetering(t *testing.T) {
	conv := New(Deps{Model: &cacheModel{}, ModelMode: "mock", Emitter: noopEmitter()})
	stream := conv.Run(context.Background(), Params{UserText: "hi", ContextID: "conv-x"})
	drainEvents(t, stream)
	if len(stream.Result().UsageEvents) != 0 {
		t.Fatalf("want no usage events without a project, got %d", len(stream.Result().UsageEvents))
	}
}

// TestCanceledContext yields a canceled terminal state.
func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conv := New(Deps{Model: &cacheModel{}, ModelMode: "mock", Emitter: noopEmitter()})
	stream := conv.Run(ctx, Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-c"})
	drainEvents(t, stream)
	if stream.Result().State != StateCanceled {
		t.Fatalf("state = %s, want canceled", stream.Result().State)
	}
}
