package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/milo-os/assistant/agentcore/mockmodel"
	"github.com/milo-os/assistant/internal/capability"
)

// globalRecorder backs every span this test binary creates. otel's global
// Tracer proxy (see this package's `tracer` var, obtained via otel.Tracer at
// package load) only ever re-delegates to a *later* otel.SetTracerProvider
// call once, process-wide (go.opentelemetry.io/otel/internal/global uses a
// sync.Once) — so tests cannot each install and tear down their own tracer
// provider. Instead, TestMain installs exactly one recorder-backed provider
// for the whole binary, and each test isolates its own spans by diffing
// against the recorder's length at the start of the test.
var globalRecorder = tracetest.NewSpanRecorder()

func TestMain(m *testing.M) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(globalRecorder))
	otel.SetTracerProvider(tp)
	os.Exit(m.Run())
}

// spansSince returns the spans ended after baseline (a length previously
// read from globalRecorder.Ended()), isolating one test's spans from
// whatever ran before it in this process.
func spansSince(baseline int) []sdktrace.ReadOnlySpan {
	ended := globalRecorder.Ended()
	if baseline >= len(ended) {
		return nil
	}
	return ended[baseline:]
}

// findSpan returns the first span with the given name, or nil.
func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// TestTurnProducesSpan proves a run wraps its top-level work in a
// "conversation.turn" span carrying the outcome, with no tool involved.
func TestTurnProducesSpan(t *testing.T) {
	baseline := len(globalRecorder.Ended())

	conv := New(Deps{
		Model:     mockmodel.New(),
		ModelMode: "mock",
		Emitter:   noopEmitter(),
	})
	stream := conv.Run(context.Background(), Params{
		UserText: "hello there", ProjectName: "demo-project", ContextID: "conv-1", TaskID: "task-1",
	})
	drainEvents(t, stream)
	if stream.Result().State != StateCompleted {
		t.Fatalf("state = %s", stream.Result().State)
	}

	spans := spansSince(baseline)
	turn := findSpan(spans, "conversation.turn")
	if turn == nil {
		names := make([]string, len(spans))
		for i, s := range spans {
			names[i] = s.Name()
		}
		t.Fatalf("expected a conversation.turn span, got: %v", names)
	}

	var sawOutcome bool
	for _, attr := range turn.Attributes() {
		if string(attr.Key) == "conversation.outcome" {
			sawOutcome = true
			if attr.Value.AsString() != string(StateCompleted) {
				t.Fatalf("conversation.outcome = %q, want %q", attr.Value.AsString(), StateCompleted)
			}
		}
	}
	if !sawOutcome {
		t.Fatal("conversation.turn span missing conversation.outcome attribute")
	}

	if findSpan(spans, "model.stream") == nil {
		t.Fatal("expected at least one model.stream span")
	}
}

// TestToolCallProducesChildSpan proves a turn that invokes a provider tool
// produces a "tool.execute" span carrying the tool name, nested under the
// "conversation.turn" span (same trace id, tool span's parent span id ==
// turn span's own span id).
func TestToolCallProducesChildSpan(t *testing.T) {
	baseline := len(globalRecorder.Ended())

	endpoint := mcpServerWithDiagnose(t)
	conv := New(Deps{
		Model:                          mockmodel.New(),
		ModelMode:                      "mock",
		Source:                         fakeSource{docs: []capability.CapabilityDocument{diagnoseDoc(endpoint)}},
		Emitter:                        noopEmitter(),
		AllowPrivateCapabilityNetworks: true,
	})
	stream := conv.Run(context.Background(), Params{
		UserText: "Please diagnose pipeline p-1", ProjectName: "demo-project", ContextID: "conv-2", TaskID: "task-2",
	})
	drainEvents(t, stream)
	if stream.Result().State != StateCompleted {
		t.Fatalf("state = %s (err=%s)", stream.Result().State, stream.Result().Error)
	}

	spans := spansSince(baseline)
	turn := findSpan(spans, "conversation.turn")
	if turn == nil {
		t.Fatal("expected a conversation.turn span")
	}
	toolSpan := findSpan(spans, "tool.execute")
	if toolSpan == nil {
		names := make([]string, len(spans))
		for i, s := range spans {
			names[i] = s.Name()
		}
		t.Fatalf("expected a tool.execute span, got: %v", names)
	}

	var sawToolName bool
	for _, attr := range toolSpan.Attributes() {
		if string(attr.Key) == "tool.name" {
			sawToolName = true
			if attr.Value.AsString() != "streamco__pipeline_diagnose" {
				t.Fatalf("tool.name = %q", attr.Value.AsString())
			}
		}
	}
	if !sawToolName {
		t.Fatal("tool.execute span missing tool.name attribute")
	}

	if toolSpan.SpanContext().TraceID() != turn.SpanContext().TraceID() {
		t.Fatal("tool.execute span is not in the same trace as conversation.turn")
	}
	if toolSpan.Parent().SpanID() != turn.SpanContext().SpanID() {
		t.Fatalf("tool.execute span's parent (%s) != conversation.turn span id (%s)",
			toolSpan.Parent().SpanID(), turn.SpanContext().SpanID())
	}
}

// TestSpansCarryNoUserContent is a privacy regression test (mirroring the
// gap-report privacy-wording test in internal/capability): no span attribute,
// across a run that has both user text and a tool call whose arguments are
// derived from that text, may contain the user's message, the tool's raw
// input, or the tool's raw output. Spans carry only infrastructure metadata —
// span names, tool.name, model.id, conversation.outcome, error booleans.
func TestSpansCarryNoUserContent(t *testing.T) {
	baseline := len(globalRecorder.Ended())

	const secretUserText = "Please diagnose pipeline p-1-super-secret-marker"
	endpoint := mcpServerWithDiagnose(t)
	conv := New(Deps{
		Model:                          mockmodel.New(),
		ModelMode:                      "mock",
		Source:                         fakeSource{docs: []capability.CapabilityDocument{diagnoseDoc(endpoint)}},
		Emitter:                        noopEmitter(),
		AllowPrivateCapabilityNetworks: true,
	})
	stream := conv.Run(context.Background(), Params{
		UserText: secretUserText, ProjectName: "demo-project", ContextID: "conv-3", TaskID: "task-3",
	})
	drainEvents(t, stream)
	result := stream.Result()
	if result.State != StateCompleted {
		t.Fatalf("state = %s (err=%s)", result.State, result.Error)
	}
	if !strings.Contains(result.Text, "CONSUMER_LAG") {
		t.Fatalf("expected the answer itself to quote tool findings (sanity check the flow ran): %q", result.Text)
	}

	spans := spansSince(baseline)
	if findSpan(spans, "conversation.turn") == nil || findSpan(spans, "tool.execute") == nil {
		t.Fatal("expected both a conversation.turn and a tool.execute span to check for leaks")
	}

	// Anything derived from the user's message or the tool's own I/O that
	// must never leak onto a span: the raw user text, the id argument the
	// mock model extracted from it, and the tool's finding.
	forbidden := []string{secretUserText, "p-1-super-secret-marker", "CONSUMER_LAG"}

	for _, s := range spans {
		checkAttrs(t, s.Name(), "name", forbidden, s.Name())
		for _, attr := range s.Attributes() {
			checkAttrs(t, s.Name(), string(attr.Key), forbidden, attr.Value.Emit())
		}
		for _, ev := range s.Events() {
			checkAttrs(t, s.Name(), "event.name", forbidden, ev.Name)
			for _, attr := range ev.Attributes {
				checkAttrs(t, s.Name(), string(attr.Key), forbidden, attr.Value.Emit())
			}
		}
		if s.Status().Description != "" {
			checkAttrs(t, s.Name(), "status.description", forbidden, s.Status().Description)
		}
	}
}

func checkAttrs(t *testing.T, spanName, field string, forbidden []string, value string) {
	t.Helper()
	for _, f := range forbidden {
		if strings.Contains(value, f) {
			t.Fatalf("span %q field %q leaks user/tool content: contains %q (value: %q)", spanName, field, f, value)
		}
	}
}
