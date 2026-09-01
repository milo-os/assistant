package server

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
)

// newTestServerWithMetrics builds a server sharing app's collectors on its
// /metrics registry — the same shape production boot wires between
// agent.Deps.Metrics and server.Deps.Metrics (see cmd/assistant/main.go).
// Each test gets its own fresh *httptest.Server AND fresh *appmetrics.Metrics
// (app is caller-supplied), so nothing here touches shared/global Prometheus
// state and tests remain safe to run with -race -count=1 in parallel.
func newTestServerWithMetrics(t *testing.T, app *appmetrics.Metrics) *httptest.Server {
	t.Helper()
	cfg, err := config.Load(config.MapGetenv(map[string]string{
		"MODEL_MODE":                "mock",
		"AUTHN_TOKENREVIEW_API_URL": "https://control-plane.test",
		"AUTHZ_SAR_API_URL":         "https://control-plane.test",
		"PUBLIC_BASE_URL":           "http://assistant.test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	log := logger.Silent()
	authn, authz := testAuth()
	app2 := New(Deps{
		Config:        cfg,
		Logger:        log,
		Authenticator: authn,
		Authorizer:    authz,
		Runner:        fakeRunner{},
		Metrics:       app,
	})
	srv := httptest.NewServer(app2)
	t.Cleanup(srv.Close)
	return srv
}

// TestApplicationMetricsRegisteredAndExposed pins that the 5 metrics added
// alongside config/components/observability/grafana-dashboard-{sre,product}.json
// and alerts.yaml
// (assistant_conversation_turn_duration_seconds, assistant_tool_call_total,
// assistant_model_call_duration_seconds, assistant_history_compaction_total,
// assistant_gap_report_total) are registered on this server's /metrics
// registry and, once recorded through the SAME *appmetrics.Metrics instance
// injected into agent.Deps elsewhere, show up in the exposition text with the
// exact label values those dashboard/alert files assume.
func TestApplicationMetricsRegisteredAndExposed(t *testing.T) {
	app := appmetrics.New()
	app.RecordTurn("completed", 250*time.Millisecond)
	app.RecordToolCall("memory_remember", "success")
	app.RecordModelCall("success", 100*time.Millisecond)
	app.RecordCompaction("failed_open")
	app.RecordGapReport("error")

	srv := newTestServerWithMetrics(t, app)

	res, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	text := string(body)

	for _, want := range []string{
		`assistant_conversation_turn_duration_seconds_bucket`,
		`state="completed"`,
		`assistant_tool_call_total{outcome="success",tool="memory_remember"} 1`,
		`assistant_model_call_duration_seconds_bucket`,
		`outcome="success"`,
		`assistant_history_compaction_total{outcome="failed_open"} 1`,
		`assistant_gap_report_total{outcome="error"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q\n--- full output ---\n%s", want, text)
		}
	}
}

// TestApplicationMetricsNilAppFallsBackWithoutPanic pins that a server built
// without an injected Metrics (Deps.Metrics nil) still boots and serves
// /metrics cleanly — newMetrics(nil) falls back to a fresh *appmetrics.Metrics
// (metrics are not an optional, nil-disables-the-feature collaborator the way
// Memory/GapReports are) rather than leaving a nil app pointer that would
// panic on MustRegister. A CounterVec/HistogramVec with no observations
// yet emits no sample lines (standard Prometheus client_golang behavior,
// matching the dashboard's "No data until this lands" note), so this test
// asserts survival and the pre-existing HTTP series, not the new families'
// text — TestApplicationMetricsRegisteredAndExposed above covers those once
// something has actually been recorded.
func TestApplicationMetricsNilAppFallsBackWithoutPanic(t *testing.T) {
	srv := newTestServer(t) // Deps.Metrics unset

	res, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	text := string(body)

	if !strings.Contains(text, "assistant_http_requests_in_flight") {
		t.Errorf("metrics endpoint did not serve normally with Deps.Metrics unset:\n%s", firstLines(text, 20))
	}
}

// TestMultipleServersDoNotDoubleRegister guards the exact panic risk the
// tracing agent's report flagged for global state: building many servers
// (each via newMetrics, each on its own *prometheus.Registry) must never
// panic with an AlreadyRegisteredError, whether or not an app Metrics is
// shared across them. Run with -race to also confirm no shared mutable state
// leaks between instances.
func TestMultipleServersDoNotDoubleRegister(t *testing.T) {
	shared := appmetrics.New()
	for i := 0; i < 3; i++ {
		newTestServerWithMetrics(t, shared)
	}
	// A server with no injected Metrics builds its own fresh one internally;
	// constructing several of those must not panic either.
	for i := 0; i < 3; i++ {
		newTestServer(t)
	}
}
