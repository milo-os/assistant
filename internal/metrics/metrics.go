// Package metrics defines the assistant's application-level Prometheus
// collectors — conversation turns, tool calls, model calls, history
// compaction, and capability-gap reports — and the [Metrics] handle used to
// record them.
//
// This package exists separately from internal/server (which owns the
// HTTP-level metrics and the /metrics endpoint) so that internal/agent,
// internal/history, and internal/capability — none of which import
// internal/server, and none of which internal/server may ever import without
// creating a cycle once it depends on the agent runner — can record these
// events without any import-direction hazard. A [Metrics] is a plain
// collector holder: it does not own a registry or an HTTP handler. The
// caller that owns the exposition endpoint (internal/server) registers a
// shared instance's collectors into its own *prometheus.Registry, and that
// same instance is injected into agent.Deps (and, through it,
// capability.ComposeOptions) so both sides observe the identical metric
// state — the same dependency-injection convention this codebase already
// uses for internal/usage.Emitter (see internal/agent.Deps.Emitter).
//
// Metrics are not an optional, nil-disables-the-feature collaborator the way
// internal/memory.Store or internal/gapreport.Store are: a *Metrics is always
// non-nil in a constructed [agent.Conversation] ([New] backs an unset
// Deps.Metrics with a fresh instance, mirroring the Logger fallback), and
// every exported Record* method also tolerates a nil receiver so a caller
// that constructs a bare struct literal without New cannot panic.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the assistant's application-level collectors, following the
// same CounterVec/HistogramVec construction and "assistant_" naming
// convention as internal/server/metrics.go. Names and label sets are pinned
// to what config/components/observability/grafana-dashboard.json and
// config/components/observability/alerts.yaml already assume — do not
// rename either without updating those files.
type Metrics struct {
	// TurnDuration is assistant_conversation_turn_duration_seconds, labeled
	// by state (completed/failed/canceled) — see internal/agent.State.
	TurnDuration *prometheus.HistogramVec
	// ToolCalls is assistant_tool_call_total, labeled by tool and outcome
	// (success/error).
	ToolCalls *prometheus.CounterVec
	// ModelCallDuration is assistant_model_call_duration_seconds, labeled by
	// outcome (success/error).
	ModelCallDuration *prometheus.HistogramVec
	// CompactionTotal is assistant_history_compaction_total, labeled by
	// outcome (success/failed_open) — see internal/agent.Conversation.maybeCompact.
	CompactionTotal *prometheus.CounterVec
	// GapReportTotal is assistant_gap_report_total, labeled by outcome
	// (success/error) — see internal/capability's report_capability_gap tool.
	GapReportTotal *prometheus.CounterVec
}

// New builds an unregistered Metrics set. The caller that owns a Prometheus
// registry (internal/server) registers its collectors via [Metrics.Collectors];
// nothing in this package touches the default global registry.
func New() *Metrics {
	return &Metrics{
		TurnDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "assistant_conversation_turn_duration_seconds",
			Help:    "Conversation turn duration in seconds by terminal state (completed/failed/canceled).",
			Buckets: prometheus.DefBuckets,
		}, []string{"state"}),
		ToolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "assistant_tool_call_total",
			Help: "Total tool calls by tool name and outcome (success/error).",
		}, []string{"tool", "outcome"}),
		ModelCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "assistant_model_call_duration_seconds",
			Help:    "Model inference call duration in seconds by outcome (success/error).",
			Buckets: prometheus.DefBuckets,
		}, []string{"outcome"}),
		CompactionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "assistant_history_compaction_total",
			Help: "Total history-compaction attempts by outcome (success/failed_open).",
		}, []string{"outcome"}),
		GapReportTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "assistant_gap_report_total",
			Help: "Total capability-gap reports by outcome (success/error).",
		}, []string{"outcome"}),
	}
}

// Collectors returns every collector so the caller that owns a
// *prometheus.Registry (internal/server) can MustRegister them alongside its
// own HTTP metrics, exposing all of them on the same /metrics endpoint.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.TurnDuration, m.ToolCalls, m.ModelCallDuration, m.CompactionTotal, m.GapReportTotal,
	}
}

// RecordTurn observes one conversation turn's duration under state
// (completed/failed/canceled).
func (m *Metrics) RecordTurn(state string, d time.Duration) {
	if m == nil {
		return
	}
	m.TurnDuration.WithLabelValues(state).Observe(d.Seconds())
}

// RecordToolCall counts one tool execution under tool/outcome
// ("success"/"error").
func (m *Metrics) RecordToolCall(tool, outcome string) {
	if m == nil {
		return
	}
	m.ToolCalls.WithLabelValues(tool, outcome).Inc()
}

// RecordModelCall observes one model inference call's duration under outcome
// ("success"/"error").
func (m *Metrics) RecordModelCall(outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.ModelCallDuration.WithLabelValues(outcome).Observe(d.Seconds())
}

// RecordCompaction counts one history-compaction attempt under outcome
// ("success"/"failed_open").
func (m *Metrics) RecordCompaction(outcome string) {
	if m == nil {
		return
	}
	m.CompactionTotal.WithLabelValues(outcome).Inc()
}

// RecordGapReport counts one capability-gap-report tool call under outcome
// ("success"/"error").
func (m *Metrics) RecordGapReport(outcome string) {
	if m == nil {
		return
	}
	m.GapReportTotal.WithLabelValues(outcome).Inc()
}
