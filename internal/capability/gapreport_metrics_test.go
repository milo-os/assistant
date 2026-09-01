package capability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/gapreport"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestGapReportRecordsSuccessMetric pins that a successful
// report_capability_gap tool call increments
// assistant_gap_report_total{outcome="success"} — the counterpart of
// TestGapReportWritesToProviderProjectNotConsumerProject, which already
// covers where the report lands.
func TestGapReportRecordsSuccessMetric(t *testing.T) {
	store := gapreport.NewMemoryStore()
	m := appmetrics.New()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
		ContextID:       "ctx-1",
		Metrics:         m,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	tool := composed.Tools[GapReportToolName("streamco")]
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"capability":"list pipelines for StreamCo","summary":"user needed a pipeline id"}`)); err != nil {
		t.Fatal(err)
	}

	if got := testutil.ToFloat64(m.GapReportTotal.WithLabelValues("success")); got != 1 {
		t.Fatalf("assistant_gap_report_total{outcome=\"success\"} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.GapReportTotal.WithLabelValues("error")); got != 0 {
		t.Fatalf("assistant_gap_report_total{outcome=\"error\"} = %v, want 0", got)
	}
}

// TestGapReportRecordsErrorMetric reuses TestGapReportBoundsSurfaceAsToolErrors'
// bound-violation fixture (an over-long capability description) to pin that a
// tool error — the store's bound-violation case here, but the same defer
// covers every error return, including malformed input and a genuine store
// failure — records assistant_gap_report_total{outcome="error"}.
func TestGapReportRecordsErrorMetric(t *testing.T) {
	store := gapreport.NewMemoryStore()
	m := appmetrics.New()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
		Metrics:         m,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	tool := composed.Tools[GapReportToolName("streamco")]
	tooLong := strings.Repeat("x", gapreport.MaxCapabilityLen+1)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"capability":"`+tooLong+`","summary":"s"}`)); err == nil {
		t.Fatal("want a too-long error")
	}

	if got := testutil.ToFloat64(m.GapReportTotal.WithLabelValues("error")); got != 1 {
		t.Fatalf("assistant_gap_report_total{outcome=\"error\"} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.GapReportTotal.WithLabelValues("success")); got != 0 {
		t.Fatalf("assistant_gap_report_total{outcome=\"success\"} = %v, want 0", got)
	}
}

// TestGapReportNilMetricsDoesNotPanic pins that ComposeOptions.Metrics unset
// (the shape every other gapreport_test.go test already uses) leaves the
// tool fully functional — Metrics is a recording aid, never a precondition
// for the feature (see reportCapabilityGapTool.metrics's doc comment).
func TestGapReportNilMetricsDoesNotPanic(t *testing.T) {
	store := gapreport.NewMemoryStore()
	composed, err := Compose(context.Background(), []CapabilityDocument{gapReportDoc("streamco-platform")}, ComposeOptions{
		GapReports:      store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	tool := composed.Tools[GapReportToolName("streamco")]
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"capability":"c","summary":"s"}`)); err != nil {
		t.Fatalf("Execute with nil Metrics: %v", err)
	}
}
