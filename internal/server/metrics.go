package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2astore "github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// isExpectedStoreErr reports whether err is a normal task-store control-flow
// sentinel (already-exists, not-found, concurrent-modification) rather than an
// operational fault, so the error metric counts only the latter.
func isExpectedStoreErr(err error) bool {
	return errors.Is(err, a2astore.ErrTaskAlreadyExists) ||
		errors.Is(err, a2astore.ErrConcurrentModification) ||
		errors.Is(err, a2a.ErrTaskNotFound)
}

// metrics holds the operational telemetry for one server instance. Each server
// owns a private *prometheus.Registry rather than the global default registry so
// tests can construct many servers without a duplicate-registration panic, and
// so this stays separate from the billing CloudEvents wire (a golden-pinned
// contract we do not touch).
type metrics struct {
	registry     *prometheus.Registry
	requests     *prometheus.CounterVec   // by handler, method, code
	duration     *prometheus.HistogramVec // by handler
	inFlight     prometheus.Gauge
	storeErrors  *prometheus.CounterVec // task-store errors by op
	readyFailure prometheus.Counter     // readiness probe failures
}

// newMetrics builds a metrics set on a fresh registry, pre-registering the Go
// runtime and process collectors for baseline pod telemetry.
func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	m := &metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "assistant_http_requests_total",
			Help: "Total HTTP requests by route, method, and response code.",
		}, []string{"handler", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "assistant_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"handler"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "assistant_http_requests_in_flight",
			Help: "In-flight HTTP requests.",
		}),
		storeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "assistant_taskstore_errors_total",
			Help: "Task-store operation errors by operation.",
		}, []string{"op"}),
		readyFailure: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "assistant_readiness_failures_total",
			Help: "Readiness-probe checks that reported not-ready.",
		}),
	}
	reg.MustRegister(
		m.requests, m.duration, m.inFlight, m.storeErrors, m.readyFailure,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// handler serves the Prometheus exposition for this server's registry.
func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// instrument wraps next with request counting, latency, and in-flight gauging.
// The route label is read from the request's matched pattern AFTER next runs
// (the mux populates r.Pattern during routing); unmatched requests bucket under
// "other" so a label cannot be spun up per bogus path.
func (m *metrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inFlight.Inc()
		defer m.inFlight.Dec()

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		handler := r.Pattern
		if handler == "" {
			handler = "other"
		}
		m.duration.WithLabelValues(handler).Observe(time.Since(start).Seconds())
		m.requests.WithLabelValues(handler, r.Method, strconv.Itoa(sw.status)).Inc()
	})
}

// statusWriter captures the response status code while transparently forwarding
// Flush (SSE) and any other optional interface via Unwrap so http.ResponseController
// reaches the underlying writer.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying ResponseWriter so http.NewResponseController can
// find Flush/Hijack/etc. — this keeps SSE streaming working through the wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush implements http.Flusher by delegating to the underlying writer. a2a-go's
// SSE writer detects streaming via a direct w.(http.Flusher) assertion, so the
// wrapper MUST surface Flush itself or SendStreamingMessage silently degrades to
// a non-streaming application/json response.
func (w *statusWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// meteredTaskStore decorates an [a2astore.Store], counting per-operation errors
// into the task-store error metric. It is transparent otherwise — the wrapped
// store's semantics (versions, sentinels) pass through unchanged. Expected
// sentinels (ErrTaskAlreadyExists, ErrTaskNotFound, ErrConcurrentModification)
// are normal control flow, not operational faults, so they are NOT counted.
type meteredTaskStore struct {
	inner   a2astore.Store
	metrics *metrics
}

func newMeteredTaskStore(inner a2astore.Store, m *metrics) *meteredTaskStore {
	return &meteredTaskStore{inner: inner, metrics: m}
}

var _ a2astore.Store = (*meteredTaskStore)(nil)

func (s *meteredTaskStore) count(op string, err error) {
	if err != nil && !isExpectedStoreErr(err) {
		s.metrics.storeErrors.WithLabelValues(op).Inc()
	}
}

func (s *meteredTaskStore) Create(ctx context.Context, task *a2a.Task) (a2astore.TaskVersion, error) {
	v, err := s.inner.Create(ctx, task)
	s.count("create", err)
	return v, err
}

func (s *meteredTaskStore) Update(ctx context.Context, req *a2astore.UpdateRequest) (a2astore.TaskVersion, error) {
	v, err := s.inner.Update(ctx, req)
	s.count("update", err)
	return v, err
}

func (s *meteredTaskStore) Get(ctx context.Context, taskID a2a.TaskID) (*a2astore.StoredTask, error) {
	st, err := s.inner.Get(ctx, taskID)
	s.count("get", err)
	return st, err
}

func (s *meteredTaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	resp, err := s.inner.List(ctx, req)
	s.count("list", err)
	return resp, err
}
