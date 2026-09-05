package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/config"
	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/logger"
)

// fakeRenamer is a scriptable [history.Renamer]: it records the last rename it
// saw and returns a fixed error (nil for success).
type fakeRenamer struct {
	err     error
	calls   int
	project string
	context string
	name    string
}

func (f *fakeRenamer) Rename(_ context.Context, projectName, contextID, name string) error {
	f.calls++
	f.project, f.context, f.name = projectName, contextID, name
	return f.err
}

func newTestServerWithRenamer(t *testing.T, renamer history.Renamer) *httptest.Server {
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
	authn, authz := testAuth()
	app := New(Deps{
		Config:        cfg,
		Logger:        logger.Silent(),
		Authenticator: authn,
		Authorizer:    authz,
		Runner:        fakeRunner{},
		Renamer:       renamer,
	})
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return srv
}

func postRename(t *testing.T, srv *httptest.Server, token string, body map[string]any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/conversations/rename", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRenameEndpoint_401WithoutToken(t *testing.T) {
	srv := newTestServerWithRenamer(t, &fakeRenamer{})
	res := postRename(t, srv, "", map[string]any{"contextId": "c1", "projectName": project, "name": "n"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

func TestRenameEndpoint_403UngrantedProject(t *testing.T) {
	renamer := &fakeRenamer{}
	srv := newTestServerWithRenamer(t, renamer)
	res := postRename(t, srv, wrongToken, map[string]any{"contextId": "c1", "projectName": project, "name": "n"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	if renamer.calls != 0 {
		t.Fatalf("calls = %d, want the store untouched on a denied request", renamer.calls)
	}
}

func TestRenameEndpoint_400BadRequests(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{"missing contextId", map[string]any{"projectName": project, "name": "n"}},
		{"missing projectName", map[string]any{"contextId": "c1", "name": "n"}},
		{"missing name", map[string]any{"contextId": "c1", "projectName": project}},
		{"whitespace-only name", map[string]any{"contextId": "c1", "projectName": project, "name": "   "}},
		{"over-long name", map[string]any{"contextId": "c1", "projectName": project,
			"name": strings.Repeat("é", history.MaxNameLen+1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renamer := &fakeRenamer{}
			srv := newTestServerWithRenamer(t, renamer)
			res := postRename(t, srv, goodToken, tt.body)
			defer res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
			if renamer.calls != 0 {
				t.Fatalf("calls = %d, want 0 for a rejected request", renamer.calls)
			}
		})
	}
}

func TestRenameEndpoint_Success(t *testing.T) {
	renamer := &fakeRenamer{}
	srv := newTestServerWithRenamer(t, renamer)
	res := postRename(t, srv, goodToken, map[string]any{
		"contextId": "c1", "projectName": project, "name": "  dfw quota escalation  "})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if renamed, _ := body["renamed"].(bool); !renamed {
		t.Fatalf("body = %+v, want renamed:true", body)
	}
	if got, _ := body["name"].(string); got != "dfw quota escalation" {
		t.Fatalf("body name = %q, want the normalized name", got)
	}
	if renamer.calls != 1 || renamer.project != project || renamer.context != "c1" {
		t.Fatalf("store saw %+v", renamer)
	}
	// Trimmed before it reaches the store, so the name it holds is the one the
	// response reported.
	if renamer.name != "dfw quota escalation" {
		t.Fatalf("store name = %q, want it trimmed", renamer.name)
	}
}

// A rename never creates a conversation, so an unknown one is a 404 rather
// than a silently successful no-op.
func TestRenameEndpoint_UnknownConversationIs404(t *testing.T) {
	srv := newTestServerWithRenamer(t, &fakeRenamer{err: history.ErrConversationNotFound})
	res := postRename(t, srv, goodToken, map[string]any{"contextId": "nope", "projectName": project, "name": "n"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

// No store wired (the same posture Compactor has): the route answers 503
// rather than the server refusing to build.
func TestRenameEndpoint_503WithoutRenamer(t *testing.T) {
	srv := newTestServerWithRenamer(t, nil)
	res := postRename(t, srv, goodToken, map[string]any{"contextId": "c1", "projectName": project, "name": "n"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}
