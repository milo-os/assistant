// Package server wires the assistant's HTTP surface: liveness at GET /healthz,
// readiness at GET /readyz, Prometheus telemetry at GET /metrics, the public
// agent card at /.well-known/agent-card.json (+ the legacy /.well-known/agent.json
// alias), and the POST /a2a JSON-RPC endpoint. The A2A protocol itself (JSON-RPC
// framing, SSE, task store) is owned by a2a-go; this package adds the mux, the
// auth middleware, request-id correlation, operational metrics, and the
// dependency wiring.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	a2astore "github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/config"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
)

// readinessTimeout bounds the dependency check behind GET /readyz so a hung
// dependency answers 503 quickly instead of stalling the probe.
const readinessTimeout = 3 * time.Second

// Deps are the fully-constructed dependencies of the HTTP app, so tests can
// inject fakes and boot wires the real graph.
type Deps struct {
	Config        *config.Config
	Logger        *slog.Logger
	Authenticator auth.Authenticator
	Authorizer    auth.Authorizer
	Runner        assistanta2a.AgentRunner

	// TaskStore backs the A2A task lifecycle. When nil the server falls back to
	// the a2a-go in-memory store (dev/tests): tasks are lost on restart. Boot
	// injects the durable Postgres store (internal/taskstore) when a conversation
	// store URL is configured.
	TaskStore a2astore.Store

	// ReadyCheck reports dependency readiness for GET /readyz. Nil ⇒ always
	// ready (dev, no external dependencies). It must be cheap and bounded — the
	// probe wraps it with a short deadline.
	ReadyCheck func(context.Context) error

	// Metrics is the shared application-level metrics handle (conversation
	// turns, tool calls, model calls, history compaction, capability-gap
	// reports) — normally the exact instance also injected into
	// agent.Deps.Metrics, so this endpoint exposes the same series the agent
	// layer records into. Nil builds a fresh, unshared instance rather than
	// leaving those series absent from /metrics.
	Metrics *appmetrics.Metrics
}

// New builds the assistant's HTTP handler from deps. The A2A JSON-RPC endpoint
// shares its task store with the auth middleware so authorization on GetTask and
// CancelTask can read the owning project off the stored task. Every route is
// wrapped with request-id correlation and Prometheus instrumentation.
func New(deps Deps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	metrics := newMetrics(deps.Metrics)

	// Task store: durable when injected, else the in-memory store. A constant
	// store user satisfies the in-memory store's Create (per-user task filtering
	// is unused — project authorization is enforced by the auth middleware, not
	// the store). Either way it is wrapped to meter operation errors.
	var store a2astore.Store = deps.TaskStore
	if store == nil {
		store = a2astore.NewInMemory(&a2astore.InMemoryStoreConfig{
			Authenticator: func(_ context.Context) (string, error) { return "assistant", nil },
		})
	}
	store = newMeteredTaskStore(store, metrics)

	executor := assistanta2a.NewExecutor(deps.Runner, logger)
	handler := a2asrv.NewHandler(executor,
		a2asrv.WithTaskStore(store),
		a2asrv.WithLogger(logger),
		// Advertise streaming; leave push notifications unconfigured so the
		// push methods reject cleanly (a2a.ErrPushNotificationNotSupported),
		// matching the TS service's "push unimplemented" behavior.
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
	)
	jsonrpc := a2asrv.NewJSONRPCHandler(handler)

	card := a2asrv.NewStaticAgentCardHandler(assistanta2a.BuildAgentCard(deps.Config))

	mw := &authMiddleware{
		authenticator: deps.Authenticator,
		authorizer:    deps.Authorizer,
		taskStore:     store,
		logger:        logger,
		next:          jsonrpc,
	}

	mux := http.NewServeMux()
	// Liveness: dependency-free, always 200 while the process is up. A
	// deployment's livenessProbe uses this — a failing dependency must NOT
	// restart the pod (that just crashloops without fixing anything).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: 200 only when dependencies are reachable, 503 otherwise. A
	// deployment's readinessProbe uses this so traffic is withheld until the DB
	// (and, in gateway mode, the model gateway) is reachable, without killing
	// the pod.
	mux.HandleFunc("GET /readyz", readyHandler(deps.ReadyCheck, metrics, logger))
	// Operational telemetry (separate from the billing CloudEvents wire).
	mux.Handle("GET /metrics", metrics.handler())
	// The card and health checks are public: clients read the card to learn the
	// auth scheme before they hold a token.
	mux.Handle("GET /.well-known/agent-card.json", card)
	mux.Handle("GET /.well-known/agent.json", card) // legacy pre-1.0 well-known path
	mux.Handle("POST /a2a", mw)

	// Outer-to-inner: tracing → request-id/logging → metrics → routes.
	// otelhttp is outermost so it extracts an inbound W3C traceparent (or
	// starts a new trace) before anything else runs, giving every downstream
	// layer — including the request-id logger — a request already inside a
	// server span. When [tracing.Setup] left the global tracer provider as
	// the no-op implementation (the default with no OTLP endpoint
	// configured), this wrapper still runs but costs a no-op span per
	// request: no exporter, no network call, no behavioral change.
	return otelhttp.NewHandler(withRequestID(metrics.instrument(mux), logger), "assistant.http")
}

// readyHandler answers GET /readyz. It runs the dependency check under a short
// deadline and maps a nil result to 200, any error to 503 (recording the
// failure for operators). A nil check means "no external dependencies" ⇒ ready.
func readyHandler(check func(context.Context) error, metrics *metrics, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if check == nil {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := check(ctx); err != nil {
			metrics.readyFailure.Inc()
			logger.Warn("http.readyz.notready", "error", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}
