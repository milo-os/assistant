package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/logger"
)

// tokenRecordingRunner captures the caller credential the turn can see, which
// is what internal/capability forwards to sanctioned provider MCP endpoints.
type tokenRecordingRunner struct{ token string }

func (r *tokenRecordingRunner) Run(ctx context.Context, req assistanta2a.RunRequest, sink assistanta2a.RunSink) assistanta2a.RunResult {
	r.token = auth.BearerTokenFromContext(ctx)
	sink.OnTextDelta("ok")
	return assistanta2a.RunResult{State: assistanta2a.RunCompleted, Text: "ok"}
}

func newTokenRecordingServer(t *testing.T) (*httptest.Server, *tokenRecordingRunner) {
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
	runner := &tokenRecordingRunner{}
	authn, authz := testAuth()
	srv := httptest.NewServer(New(Deps{
		Config:        cfg,
		Logger:        logger.Silent(),
		Authenticator: authn,
		Authorizer:    authz,
		Runner:        runner,
	}))
	t.Cleanup(srv.Close)
	return srv, runner
}

// An authenticated and authorized turn can see the caller's own credential —
// the whole point being that a provider tool may then read as that user.
func TestCallerTokenReachesTheTurn(t *testing.T) {
	srv, runner := newTokenRecordingServer(t)

	res := rpc(t, srv, goodToken, methodSendMessage, sendMessageParams(project, "hello"), 1)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if runner.token != goodToken {
		t.Fatalf("runner saw token %q, want the caller's own", runner.token)
	}
}

// A request that fails authentication or project authorization never reaches
// the runner, so no credential is stashed on a context the turn could use.
func TestCallerTokenNotStashedOnRejectedRequests(t *testing.T) {
	for _, tc := range []struct {
		name       string
		token      string
		project    string
		wantStatus int
	}{
		{"unknown token", "nope", project, http.StatusUnauthorized},
		{"ungranted project", wrongToken, project, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, runner := newTokenRecordingServer(t)
			res := rpc(t, srv, tc.token, methodSendMessage, sendMessageParams(tc.project, "hello"), 1)
			defer res.Body.Close()
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			if runner.token != "" {
				t.Fatalf("rejected request left a credential downstream: %q", runner.token)
			}
		})
	}
}

// A context with no authenticated request behind it (every non-HTTP entry
// point) yields no credential rather than a zero-value surprise.
func TestBearerTokenFromContext_EmptyByDefault(t *testing.T) {
	if got := auth.BearerTokenFromContext(context.Background()); got != "" {
		t.Fatalf("bare context yielded token %q", got)
	}
	ctx := auth.ContextWithBearerToken(context.Background(), "")
	if got := auth.BearerTokenFromContext(ctx); got != "" {
		t.Fatalf("empty token round-tripped as %q", got)
	}
}
