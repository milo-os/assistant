package agent

import (
	"testing"

	"github.com/milo-os/assistant/agentcore/mockmodel"
	"github.com/milo-os/assistant/internal/capability"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// histogramSampleCount registers vec on a scratch registry, gathers it, and
// returns the observation count (the "_count" series) for the child with the
// given single label value. It fails the test if the family or child is
// missing, so callers get a clear diagnostic instead of a silent 0.
func histogramSampleCount(t *testing.T, vec *prometheus.HistogramVec, familyName, labelName, labelValue string) uint64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(vec)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != familyName {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == labelName && lp.GetValue() == labelValue {
					return metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	t.Fatalf("no %s{%s=%q} series found", familyName, labelName, labelValue)
	return 0
}

// counterValue is [histogramSampleCount]'s CounterVec counterpart. Unlike
// histogramSampleCount it does NOT fail when the series is absent: a
// CounterVec child that was never incremented emits no sample at all (normal
// Prometheus client_golang behavior — see the "No data" note in
// docs/dashboards-and-alerts.md), and callers use this to assert a
// not-yet-observed outcome is legitimately 0.
func counterValue(t *testing.T, vec *prometheus.CounterVec, familyName string, labels map[string]string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(vec)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != familyName {
			continue
		}
		for _, metric := range mf.GetMetric() {
			if labelsMatch(metric, labels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(metric *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range metric.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// TestMetricsRecordsCompletedTurn pins that a completed turn observes
// assistant_conversation_turn_duration_seconds{state="completed"} at the
// same point endTurnSpan finalizes the turn span (Stream.finalize).
func TestMetricsRecordsCompletedTurn(t *testing.T) {
	m := appmetrics.New()
	conv := New(Deps{
		Model:     mockmodel.New(),
		ModelMode: "mock",
		Emitter:   noopEmitter(),
		Metrics:   m,
	})

	res := runTurn(t, conv, Params{
		UserText: "hello", ProjectName: "demo-project", ContextID: "conv-metrics-turn", TaskID: "t1",
	})
	if res.State != StateCompleted {
		t.Fatalf("state = %s (err=%s)", res.State, res.Error)
	}

	if got := histogramSampleCount(t, m.TurnDuration, "assistant_conversation_turn_duration_seconds", "state", "completed"); got != 1 {
		t.Fatalf("completed turn samples = %d, want 1", got)
	}
}

// TestMetricsRecordsToolCall pins that a provider tool call increments
// assistant_tool_call_total{tool,outcome="success"} in the tracedTools
// decorator, alongside the tool.execute span.
func TestMetricsRecordsToolCall(t *testing.T) {
	endpoint := mcpServerWithDiagnose(t)
	m := appmetrics.New()
	conv := New(Deps{
		Model:                          mockmodel.New(),
		ModelMode:                      "mock",
		Source:                         fakeSource{docs: []capability.CapabilityDocument{diagnoseDoc(endpoint)}},
		Emitter:                        noopEmitter(),
		AllowPrivateCapabilityNetworks: true,
		Metrics:                        m,
	})

	res := runTurn(t, conv, Params{
		UserText: "Please diagnose pipeline p-1", ProjectName: "demo-project", ContextID: "conv-metrics-tool", TaskID: "t1",
	})
	if res.State != StateCompleted {
		t.Fatalf("state = %s (err=%s)", res.State, res.Error)
	}

	got := counterValue(t, m.ToolCalls, "assistant_tool_call_total",
		map[string]string{"tool": "streamco__pipeline_diagnose", "outcome": "success"})
	if got != 1 {
		t.Fatalf("tool call count = %v, want 1", got)
	}
}

// TestMetricsRecordsModelCall pins that every model.Stream call observes
// assistant_model_call_duration_seconds{outcome="success"} in tracedModel.
func TestMetricsRecordsModelCall(t *testing.T) {
	m := appmetrics.New()
	conv := New(Deps{
		Model:     mockmodel.New(),
		ModelMode: "mock",
		Emitter:   noopEmitter(),
		Metrics:   m,
	})

	res := runTurn(t, conv, Params{
		UserText: "hello", ProjectName: "demo-project", ContextID: "conv-metrics-model", TaskID: "t1",
	})
	if res.State != StateCompleted {
		t.Fatalf("state = %s (err=%s)", res.State, res.Error)
	}

	if got := histogramSampleCount(t, m.ModelCallDuration, "assistant_model_call_duration_seconds", "outcome", "success"); got == 0 {
		t.Fatalf("model call samples = %d, want at least 1", got)
	}
}

// TestMetricsRecordsCompactionSuccess pins that a successful compaction
// (History.Compact called and returning nil) increments
// assistant_history_compaction_total{outcome="success"} — the mirror image
// of TestMetricsRecordsCompactionFailedOpen below, and of
// TestCompactionFiresAboveThreshold in summarize_test.go.
func TestMetricsRecordsCompactionSuccess(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	m := appmetrics.New()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200, Metrics: m})

	params := Params{ProjectName: "demo-project", ContextID: "conv-metrics-compact-ok"}
	fillTurns(t, conv, params, 7) // crosses the threshold on turn 7, see summarize_test.go

	if store.compactCalls != 1 {
		t.Fatalf("setup: compactCalls = %d, want 1", store.compactCalls)
	}
	if got := counterValue(t, m.CompactionTotal, "assistant_history_compaction_total", map[string]string{"outcome": "success"}); got != 1 {
		t.Fatalf("success compaction count = %v, want 1", got)
	}
	if got := counterValue(t, m.CompactionTotal, "assistant_history_compaction_total", map[string]string{"outcome": "failed_open"}); got != 0 {
		t.Fatalf("failed_open compaction count = %v, want 0", got)
	}
}

// TestMetricsRecordsCompactionFailedOpen reuses TestSummarizeFailureFallsOpenToTruncate's
// fixture (a model that errors on the summarize system prompt specifically)
// to pin that the fail-open branch of maybeCompact records
// assistant_history_compaction_total{outcome="failed_open"} — the ONLY
// dashboard/alert signal for this silent-degradation path (see
// docs/dashboards-and-alerts.md's "Compaction failed-open rate" panel and the
// AssistantCompactionFailingOpen alert).
func TestMetricsRecordsCompactionFailedOpen(t *testing.T) {
	model := &compactModel{failSummarize: true}
	store := newSpyStore()
	m := appmetrics.New()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200, Metrics: m})

	params := Params{ProjectName: "demo-project", ContextID: "conv-metrics-compact-failopen"}
	res := fillTurns(t, conv, params, 7) // crosses the threshold on turn 7

	if res.State != StateCompleted {
		t.Fatalf("turn state = %s, want completed despite a failed summarize call", res.State)
	}
	if store.compactCalls != 0 {
		t.Fatalf("compactCalls = %d, want 0", store.compactCalls)
	}
	if got := counterValue(t, m.CompactionTotal, "assistant_history_compaction_total", map[string]string{"outcome": "failed_open"}); got != 1 {
		t.Fatalf("failed_open compaction count = %v, want 1", got)
	}
	if got := counterValue(t, m.CompactionTotal, "assistant_history_compaction_total", map[string]string{"outcome": "success"}); got != 0 {
		t.Fatalf("success compaction count = %v, want 0", got)
	}
}
