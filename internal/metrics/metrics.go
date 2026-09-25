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
// to what config/components/observability/grafana-dashboard-{sre,product}.json
// and config/components/observability/alerts.yaml already assume — do not
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

	// ── Capability source (CRD) ───────────────────────────────
	//
	// None of the series below carries a project label, and none ever should.
	// Project cardinality is set by tenant count, a number this repository
	// neither controls nor can bound, and a per-tenant time series is how a
	// metrics backend falls over. An operator debugging one project reads the
	// logs, which already carry projectName on every capability line.

	// CapabilityFetchTotal is assistant_capability_fetch_total, labeled by
	// outcome (hit/miss/refresh_failed/stale_served/empty). One counter answers
	// both "is the cache working" and "is the control plane failing":
	// stale_served is the alertable series — a nonzero rate means projects are
	// running on configuration the control plane can no longer confirm.
	CapabilityFetchTotal *prometheus.CounterVec
	// CapabilityFetchDuration is assistant_capability_fetch_duration_seconds,
	// labeled by outcome (ok/error), over the LIST itself. The whole point of
	// the cache is to take this latency off the turn path; that claim is only
	// provable if the uncommon path is measured.
	CapabilityFetchDuration *prometheus.HistogramVec
	// CapabilityCacheEntryAge is assistant_capability_cache_entry_age_seconds,
	// observed at SERVE time. A histogram, not a gauge: entries are per project,
	// so there is no single "the cache" age to gauge, and the number an operator
	// wants during an outage is the p99 of what is actually being served.
	CapabilityCacheEntryAge prometheus.Histogram
	// CapabilityCacheEntries is assistant_capability_cache_entries, the resident
	// entry count — the series that says whether the bounded cache is at its cap
	// and evicting live entries.
	CapabilityCacheEntries prometheus.Gauge

	// CapabilityStatusWriteTotal is assistant_capability_status_write_total,
	// labeled by outcome (ok/error/forbidden/dropped), over the status
	// conditions written back onto CapabilityBinding objects. Three of the four
	// outcomes are distinct questions, which is why they are not one "error":
	// "forbidden" is the standing open question about whether Milo evaluates a
	// status write as capabilitybindings.patch on the parent or as a distinct
	// subresource permission — if it is the latter, EVERY write 403s in
	// production and nothing else is visibly wrong, so it must not hide inside
	// a generic error rate. "dropped" is the bounded queue shedding under a
	// control-plane outage, which is the designed degradation ("status not
	// updated") and not a fault. No project label, for the reason above.
	CapabilityStatusWriteTotal *prometheus.CounterVec

	// CapabilityScopeDropped is assistant_capability_scope_dropped_total,
	// labeled by reason ("mismatch"), incremented by capability.ScopeDocuments.
	// Tenant isolation is the one boundary that must not be observable only by
	// grepping logs: "mismatch" is a document whose own namespace named another
	// project (a Source bug or an attempted crossing), and any non-zero value is
	// worth an alert. It is labeled rather than bare so a second reason can be
	// added without breaking a dashboard.
	CapabilityScopeDropped *prometheus.CounterVec
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
		CapabilityFetchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "assistant_capability_fetch_total",
			Help: "Total capability-document lookups by outcome (hit/miss/refresh_failed/stale_served/empty).",
		}, []string{"outcome"}),
		CapabilityFetchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "assistant_capability_fetch_duration_seconds",
			Help:    "Capability-binding LIST duration in seconds by outcome (ok/error).",
			Buckets: prometheus.DefBuckets,
		}, []string{"outcome"}),
		CapabilityCacheEntryAge: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "assistant_capability_cache_entry_age_seconds",
			Help: "Age in seconds of the capability cache entry served, observed at serve time.",
			// The TTL is 60s, so a healthy deployment lives entirely in the
			// first few buckets; everything above 60 is a stale serve, and the
			// long tail is what an outage looks like.
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 900, 3600},
		}),
		CapabilityCacheEntries: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "assistant_capability_cache_entries",
			Help: "Capability cache entries currently resident (one per active project).",
		}),
		CapabilityStatusWriteTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "assistant_capability_status_write_total",
			Help: "Total CapabilityBinding status-condition writes by outcome (ok/error/forbidden/dropped).",
		}, []string{"outcome"}),
		CapabilityScopeDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "assistant_capability_scope_dropped_total",
			Help: "Capability documents dropped by the tenant-scope gate, by reason (mismatch).",
		}, []string{"reason"}),
	}
}

// Collectors returns every collector so the caller that owns a
// *prometheus.Registry (internal/server) can MustRegister them alongside its
// own HTTP metrics, exposing all of them on the same /metrics endpoint.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.TurnDuration, m.ToolCalls, m.ModelCallDuration, m.CompactionTotal, m.GapReportTotal,
		m.CapabilityFetchTotal, m.CapabilityFetchDuration, m.CapabilityCacheEntryAge,
		m.CapabilityCacheEntries, m.CapabilityStatusWriteTotal, m.CapabilityScopeDropped,
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

// RecordCapabilityFetch counts one capability-document lookup under outcome
// ("hit"/"miss"/"refresh_failed"/"stale_served"/"empty"). A single lookup can
// record more than one: a failed refresh that falls back to a retained entry
// records refresh_failed AND stale_served, because the two questions
// ("is the control plane answering" and "are users on old config") have
// different answers and different alerts.
func (m *Metrics) RecordCapabilityFetch(outcome string) {
	if m == nil {
		return
	}
	m.CapabilityFetchTotal.WithLabelValues(outcome).Inc()
}

// RecordCapabilityFetchDuration observes one capability LIST round-trip under
// outcome ("ok"/"error").
func (m *Metrics) RecordCapabilityFetchDuration(outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.CapabilityFetchDuration.WithLabelValues(outcome).Observe(d.Seconds())
}

// RecordCapabilityCacheEntryAge observes the age of the cache entry being
// served. Called at serve time only — an entry nobody reads has no age worth
// reporting.
func (m *Metrics) RecordCapabilityCacheEntryAge(age time.Duration) {
	if m == nil {
		return
	}
	m.CapabilityCacheEntryAge.Observe(age.Seconds())
}

// SetCapabilityCacheEntries publishes the resident capability-cache entry count.
func (m *Metrics) SetCapabilityCacheEntries(n int) {
	if m == nil {
		return
	}
	m.CapabilityCacheEntries.Set(float64(n))
}

// RecordCapabilityStatusWrite counts one CapabilityBinding status-condition
// write attempt under outcome ("ok"/"error"/"forbidden"/"dropped"). "dropped"
// is recorded without any API call having been made — the bounded queue shed
// the write rather than let a control-plane outage back up onto the request
// path.
func (m *Metrics) RecordCapabilityStatusWrite(outcome string) {
	if m == nil {
		return
	}
	m.CapabilityStatusWriteTotal.WithLabelValues(outcome).Inc()
}

// RecordCapabilityScopeDropped counts one document dropped by the tenant-scope
// gate under reason ("mismatch").
func (m *Metrics) RecordCapabilityScopeDropped(reason string) {
	if m == nil {
		return
	}
	m.CapabilityScopeDropped.WithLabelValues(reason).Inc()
}
