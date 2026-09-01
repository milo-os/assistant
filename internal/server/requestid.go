package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// requestIDHeader is the inbound/outbound correlation header. An upstream proxy
// or client may supply it; otherwise the middleware mints one.
const requestIDHeader = "X-Request-Id"

type ctxKey int

const (
	requestIDKey ctxKey = iota
	loggerKey
)

// newRequestID returns a 128-bit hex request id. crypto/rand keeps ids
// unguessable and dependency-free (no uuid import); a rand failure falls back to
// a timestamp so a request is never left uncorrelated.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ts-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// withRequestID is the outermost middleware: it establishes a request id
// (honoring an inbound X-Request-Id) and a request-scoped logger carrying it,
// stashes both on the context for downstream layers, and echoes the id on the
// response so a client can correlate. It logs a single structured line per
// request at completion — method, path, status, duration, request id — and
// deliberately NEVER logs the body, so prompt/PII content is not written at info
// level.
func withRequestID(next http.Handler, base *slog.Logger) http.Handler {
	if base == nil {
		base = slog.New(slog.DiscardHandler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		reqLog := base.With("requestId", id)

		ctx := context.WithValue(r.Context(), requestIDKey, id)
		ctx = context.WithValue(ctx, loggerKey, reqLog)

		w.Header().Set(requestIDHeader, id)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		start := time.Now()
		next.ServeHTTP(sw, r.WithContext(ctx))

		// Path is a fixed route surface (/a2a, /healthz, …); no query string and
		// no body are logged, so no prompt content leaks at info level.
		reqLog.Info("http.request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"durationMs", time.Since(start).Milliseconds())
	})
}

// RequestIDFromContext returns the request id threaded onto ctx by the request-id
// middleware, or "" when absent. Downstream layers can attach it to their own
// logs to correlate with the HTTP request line.
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// LoggerFromContext returns the request-scoped logger (carrying requestId) that
// the middleware attached, or fallback when none is present. It lets the agent
// layer emit request-correlated logs without importing the HTTP wiring.
func LoggerFromContext(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return fallback
}
