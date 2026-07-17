package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
)

// newTestServerWithReady builds a server with an injected readiness check so the
// /readyz probe can be exercised in both states.
func newTestServerWithReady(t *testing.T, ready func(context.Context) error) *httptest.Server {
	t.Helper()
	cfg, err := config.Load(config.MapGetenv(map[string]string{
		"AUTH_MODE":       "dev",
		"AUTH_DEV_TOKENS": goodToken + ":alice:" + project,
		"MODEL_MODE":      "mock",
		"PUBLIC_BASE_URL": "http://assistant.test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	log := logger.Silent()
	app := New(Deps{
		Config:        cfg,
		Logger:        log,
		Authenticator: mustAuthenticator(t, cfg, log),
		Authorizer:    auth.NewAuthorizer(cfg, log),
		Runner:        fakeRunner{},
		ReadyCheck:    ready,
	})
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return srv
}

// ── readiness (C) ──────────────────────────────────────────────

func TestReadyz(t *testing.T) {
	t.Run("nil check is always ready", func(t *testing.T) {
		srv := newTestServer(t) // no ReadyCheck ⇒ 200
		res, err := srv.Client().Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
	})

	t.Run("dependency down returns 503", func(t *testing.T) {
		srv := newTestServerWithReady(t, func(context.Context) error {
			return errors.New("database unreachable")
		})
		res, err := srv.Client().Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", res.StatusCode)
		}
	})

	t.Run("dependency up returns 200", func(t *testing.T) {
		srv := newTestServerWithReady(t, func(context.Context) error { return nil })
		res, err := srv.Client().Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
	})
}

// TestHealthzIndependentOfReadiness pins liveness staying 200 even when
// readiness reports down — a failing dependency must not restart the pod.
func TestHealthzIndependentOfReadiness(t *testing.T) {
	srv := newTestServerWithReady(t, func(context.Context) error {
		return errors.New("db down")
	})
	res, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200 regardless of readiness", res.StatusCode)
	}
}

// ── metrics (E) ────────────────────────────────────────────────

func TestMetricsEndpoint(t *testing.T) {
	srv := newTestServer(t)
	// Drive one authenticated /a2a request so a request metric is recorded.
	res := rpc(t, srv, goodToken, "SendMessage", sendMessageParams(project, "hi"), "m")
	res.Body.Close()

	metricsRes, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer metricsRes.Body.Close()
	body, _ := io.ReadAll(metricsRes.Body)
	text := string(body)

	for _, want := range []string{
		"assistant_http_requests_total",
		"assistant_http_request_duration_seconds",
		"assistant_http_requests_in_flight",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	// The /a2a POST must be counted under its route pattern.
	if !strings.Contains(text, `handler="POST /a2a"`) {
		t.Errorf("metrics missing a POST /a2a request series:\n%s", firstLines(text, 40))
	}
}

// ── request-id / correlation (F) ───────────────────────────────

func TestRequestIDGenerated(t *testing.T) {
	srv := newTestServer(t)
	res, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("X-Request-Id"); got == "" {
		t.Fatal("response missing generated X-Request-Id header")
	}
}

func TestRequestIDHonored(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	req.Header.Set("X-Request-Id", "trace-abc-123")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("X-Request-Id"); got != "trace-abc-123" {
		t.Fatalf("echoed request id = %q, want the inbound value", got)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
