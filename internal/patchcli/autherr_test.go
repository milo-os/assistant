package patchcli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// The service rejects auth failures at HTTP level — 401/403 with a JSON body —
// not as JSON-RPC errors, and a2a-go's transport reports only "unexpected HTTP
// status: …" once it has discarded that body. These tests cover that path,
// which the in-protocol fake in run_test.go cannot reach.

// newRejectingService starts a service whose /a2a always answers with one HTTP
// status and JSON error body, the way the real service answers an unknown
// token or an ungranted project. The agent card is served normally, since the
// real service serves it publicly.
func newRejectingService(t *testing.T, status int, body string) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/a2a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	card := &a2a.AgentCard{
		Name:        "Patch",
		Description: "Patch is the Datum Cloud assistant.",
		Version:     "0.1.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(srv.URL+"/a2a", a2a.TransportProtocolJSONRPC),
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		SecuritySchemes:    a2a.NamedSecuritySchemes{"bearer": a2a.HTTPAuthSecurityScheme{Scheme: "Bearer"}},
		Skills:             []a2a.AgentSkill{{ID: "project-assistant", Name: "Project assistant", Description: "d"}},
	}
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	return srv.URL
}

func TestRun_HTTPUnauthorized_ReportsServiceMessage(t *testing.T) {
	base := newRejectingService(t, http.StatusUnauthorized,
		`{"error":"Unknown or invalid bearer token"}`)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "stale"})

	var io capture
	code := Run(context.Background(),
		[]string{"chat", "hello", "--project", "demo-project"}, env, &io)

	if code != 1 {
		t.Fatalf("code = %d, want 1\nstderr: %s", code, io.err.String())
	}
	stderr := io.err.String()
	if !strings.Contains(stderr, "unauthorized") {
		t.Errorf("stderr should classify the failure as unauthorized: %q", stderr)
	}
	// The whole point of the fix: the service's own words, which a2a-go drops.
	if !strings.Contains(stderr, "Unknown or invalid bearer token") {
		t.Errorf("stderr should carry the service's message: %q", stderr)
	}
}

func TestRun_HTTPForbidden_ReportsServiceMessage(t *testing.T) {
	base := newRejectingService(t, http.StatusForbidden,
		`{"error":"Token does not grant access to project \"demo-project\""}`)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})

	var io capture
	code := Run(context.Background(),
		[]string{"chat", "hello", "--project", "demo-project"}, env, &io)

	if code != 1 {
		t.Fatalf("code = %d, want 1\nstderr: %s", code, io.err.String())
	}
	stderr := io.err.String()
	if !strings.Contains(stderr, "forbidden") {
		t.Errorf("stderr should classify the failure as forbidden: %q", stderr)
	}
	if !strings.Contains(stderr, `does not grant access to project "demo-project"`) {
		t.Errorf("stderr should name the project the token lacks: %q", stderr)
	}
}

func TestFriendlyError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		errs   *httpErrRecorder
		want   string
		reject string // must NOT appear
	}{
		{
			name: "in-protocol sentinel still wins",
			err:  a2a.NewError(a2a.ErrUnauthenticated, "no bearer token"),
			errs: nil,
			want: "unauthorized",
		},
		{
			name: "http 401 with a service message",
			err:  errors.New("unexpected HTTP status: 401 Unauthorized"),
			errs: &httpErrRecorder{status: 401, msg: "Missing bearer token"},
			want: "Missing bearer token",
		},
		{
			name:   "http 403 message replaces the opaque status",
			err:    errors.New("unexpected HTTP status: 403 Forbidden"),
			errs:   &httpErrRecorder{status: 403, msg: "Method not permitted"},
			want:   "Method not permitted",
			reject: "unexpected HTTP status",
		},
		{
			name: "no recorded response falls back to the error",
			err:  errors.New("dial tcp: connection refused"),
			errs: &httpErrRecorder{},
			want: "dial tcp: connection refused",
		},
		{
			name: "nil recorder falls back to the error",
			err:  errors.New("dial tcp: connection refused"),
			errs: nil,
			want: "dial tcp: connection refused",
		},
		{
			name: "other status with a body reports the body",
			err:  errors.New("unexpected HTTP status: 413 Request Entity Too Large"),
			errs: &httpErrRecorder{status: 413, msg: "Request body too large"},
			want: "Request body too large",
		},
		{
			name: "other status without a body falls back to the error",
			err:  errors.New("unexpected HTTP status: 502 Bad Gateway"),
			errs: &httpErrRecorder{status: 502},
			want: "unexpected HTTP status: 502 Bad Gateway",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := friendlyError(tc.err, tc.errs)
			if !strings.Contains(got, tc.want) {
				t.Errorf("friendlyError = %q, want it to contain %q", got, tc.want)
			}
			if tc.reject != "" && strings.Contains(got, tc.reject) {
				t.Errorf("friendlyError = %q, should not contain %q", got, tc.reject)
			}
		})
	}
}

// A non-JSON error body (a gateway's HTML, say) should still reach the user
// rather than being swallowed for failing to parse.
func TestErrorMessageFrom_NonJSONBody(t *testing.T) {
	res := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       http.NoBody,
	}
	if got := errorMessageFrom(res); got != "" {
		t.Errorf("empty body = %q, want \"\"", got)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream connect error"))
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if got := errorMessageFrom(resp); got != "upstream connect error" {
		t.Errorf("message = %q, want the raw body", got)
	}
	// The body must survive being read for the message.
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(rest) != "upstream connect error" {
		t.Errorf("body after inspection = %q, want it intact", rest)
	}
}
