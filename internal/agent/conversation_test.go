package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		// The test MCP server binds loopback; allow private so the SSRF guard
		// (safe-default: block loopback/private) does not refuse it.
		AllowPrivateCapabilityNetworks: true,
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

	// Usage: token meters aggregated across BOTH model steps (tool-call step +
	// answer step, 42/23 each) plus exactly one tool-invocation event. The
	// multi-step TOTAL (84/46) is the billing-critical number the sink golden
	// pins — emitting only the final step (42/23) is the TS under-billing bug.
	byMeter := map[string]string{}
	var toolEvents int
	for _, e := range result.UsageEvents {
		byMeter[e.MeterName] = e.Value
		if strings.HasSuffix(e.MeterName, "tool-invocations") {
			toolEvents++
			if e.Dimensions["service"] != "streaming.streamco.example" {
				t.Fatalf("tool event service dim = %q", e.Dimensions["service"])
			}
		} else if e.Dimensions["model"] != "patch-mock-v0" {
			t.Fatalf("token meter %s model dim = %q", e.MeterName, e.Dimensions["model"])
		}
	}
	if byMeter["assistant.miloapis.com/conversation/input-tokens"] != "84" {
		t.Fatalf("input-tokens total = %q, want 84 (2 steps x 42)", byMeter["assistant.miloapis.com/conversation/input-tokens"])
	}
	if byMeter["assistant.miloapis.com/conversation/output-tokens"] != "46" {
		t.Fatalf("output-tokens total = %q, want 46 (2 steps x 23)", byMeter["assistant.miloapis.com/conversation/output-tokens"])
	}
	if byMeter["assistant.miloapis.com/conversation/messages"] != "1" {
		t.Fatalf("messages = %q, want 1", byMeter["assistant.miloapis.com/conversation/messages"])
	}
	if toolEvents != 1 {
		t.Fatalf("want 1 tool-invocation event, got %d", toolEvents)
	}
	if result.Usage.ToolInvocationEventCount != 1 {
		t.Fatalf("ToolInvocationEventCount = %d", result.Usage.ToolInvocationEventCount)
	}
}

// cacheModel is a one-step model that reports cache read/write tokens.
type cacheModel struct {
	headers   map[string]string
	maxTokens int
}

func (m *cacheModel) ModelID() string { return "cache-pin" }
func (m *cacheModel) Stream(_ context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	m.headers = req.Headers
	m.maxTokens = req.MaxOutputTokens
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

// TestDefaultMaxOutputTokens verifies the 4096 TS-parity default is applied
// when Deps leaves MaxOutputTokens unset, and an explicit value wins.
func TestDefaultMaxOutputTokens(t *testing.T) {
	defModel := &cacheModel{}
	drainEvents(t, New(Deps{Model: defModel, ModelMode: "mock", Emitter: noopEmitter()}).
		Run(context.Background(), Params{UserText: "hi", ProjectName: "p", ContextID: "c"}))
	if defModel.maxTokens != DefaultMaxOutputTokens {
		t.Fatalf("default max tokens = %d, want %d", defModel.maxTokens, DefaultMaxOutputTokens)
	}

	explicitModel := &cacheModel{}
	drainEvents(t, New(Deps{Model: explicitModel, ModelMode: "mock", Emitter: noopEmitter(), MaxOutputTokens: 512}).
		Run(context.Background(), Params{UserText: "hi", ProjectName: "p", ContextID: "c"}))
	if explicitModel.maxTokens != 512 {
		t.Fatalf("explicit max tokens = %d, want 512", explicitModel.maxTokens)
	}
}

// blockingModel returns a stream whose Recv blocks until the request context is
// canceled, then reports the context error — standing in for a real model that
// hangs (never streams, never finishes) so the per-turn deadline is what ends
// the turn.
type blockingModel struct{}

func (blockingModel) ModelID() string { return "blocking" }
func (blockingModel) Stream(ctx context.Context, _ agentcore.Request) (agentcore.StreamReader, error) {
	return &blockingReader{ctx: ctx}, nil
}

type blockingReader struct{ ctx context.Context }

func (r *blockingReader) Recv() (agentcore.StreamPart, error) {
	<-r.ctx.Done()
	return agentcore.StreamPart{}, r.ctx.Err()
}
func (r *blockingReader) Close() error { return nil }

// TestPerTurnDeadlineCancelsStuckModel pins the per-turn wall-clock bound: a
// model that never completes is cut off at TurnTimeout and the turn ends
// canceled (distinct from a normal completion), instead of pinning the request
// forever. Nothing streamed, so no tokens are billed.
func TestPerTurnDeadlineCancelsStuckModel(t *testing.T) {
	conv := New(Deps{
		Model: blockingModel{}, ModelMode: "mock", Emitter: noopEmitter(),
		TurnTimeout: 40 * time.Millisecond,
	})
	start := time.Now()
	stream := conv.Run(context.Background(), Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-timeout"})
	drainEvents(t, stream)
	elapsed := time.Since(start)
	res := stream.Result()

	if res.State != StateCanceled {
		t.Fatalf("state = %s, want canceled (a per-turn timeout must not read as completed)", res.State)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("turn ran %v — the per-turn deadline did not fire", elapsed)
	}
	// A turn that produced no step-finish bills nothing.
	if len(res.UsageEvents) != 0 {
		t.Fatalf("a timed-out turn with no completed step must bill nothing, got %d events", len(res.UsageEvents))
	}
}

// TestRetryRecoversInConversation pins that the conversation wires the loop's
// transient-failure retry: a first-attempt rate limit (retryable) recovers on
// the retry and the turn completes, billing only the successful step.
func TestRetryRecoversInConversation(t *testing.T) {
	model := &scriptModel{turns: []scriptTurn{
		{err: agentcore.NewModelError(agentcore.ErrClassRateLimited, 0, errors.New("429 rate limited"))},
		{parts: []agentcore.StreamPart{
			{Kind: agentcore.StreamPartTextDelta, Text: "recovered"},
			{Kind: agentcore.StreamPartStepFinish, Usage: agentcore.Usage{Input: 12, Output: 7}, FinishReason: agentcore.FinishStop},
		}},
	}}
	conv := New(Deps{
		Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		RetryBaseDelay: time.Microsecond, RetryMaxDelay: time.Millisecond,
	})
	stream := conv.Run(context.Background(), Params{UserText: "go", ProjectName: "demo-project", ContextID: "c"})
	drainEvents(t, stream)
	res := stream.Result()

	if res.State != StateCompleted {
		t.Fatalf("state = %s, want completed after a retried rate limit (err=%s)", res.State, res.Error)
	}
	if res.Text != "recovered" {
		t.Fatalf("text = %q, want the retried attempt's answer", res.Text)
	}
	if model.call != 2 {
		t.Fatalf("want 2 attempts (retry once), got %d", model.call)
	}
	byMeter := meterValues(res.UsageEvents)
	if byMeter[usage.MeterInputTokens] != "12" || byMeter[usage.MeterOutputTokens] != "7" {
		t.Fatalf("only the successful attempt bills: input=%q output=%q, want 12/7",
			byMeter[usage.MeterInputTokens], byMeter[usage.MeterOutputTokens])
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
