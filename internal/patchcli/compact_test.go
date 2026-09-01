package patchcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestCompactService starts a bare HTTP server implementing just POST
// /v1/compact (the CLI never talks A2A for this command, so there's no need
// for the full a2a-go server stack newTestService sets up for chat). It
// checks the bearer token against a fixed "good" value and replies according
// to reply, recording the last decoded request body for assertions.
type compactRequestSpy struct {
	ContextID   string `json:"contextId"`
	ProjectName string `json:"projectName"`
}

func newTestCompactService(t *testing.T, reply map[string]any, status int) (string, *compactRequestSpy) {
	t.Helper()
	spy := &compactRequestSpy{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/compact", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Missing bearer token"})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(spy)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(reply)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, spy
}

func TestRequestCompact_Success(t *testing.T) {
	base, spy := newTestCompactService(t, map[string]any{"compacted": true}, http.StatusOK)
	err := requestCompact(context.Background(), base, StaticToken("good"), "demo-project", "ctx-1")
	if err != nil {
		t.Fatalf("requestCompact: %v", err)
	}
	if spy.ContextID != "ctx-1" || spy.ProjectName != "demo-project" {
		t.Fatalf("request body = %+v", spy)
	}
}

func TestRequestCompact_NothingToCompact(t *testing.T) {
	base, _ := newTestCompactService(t, map[string]any{"compacted": false, "reason": "nothing to compact"}, http.StatusOK)
	err := requestCompact(context.Background(), base, StaticToken("good"), "demo-project", "ctx-1")
	if err != ErrNothingToCompact {
		t.Fatalf("err = %v, want ErrNothingToCompact", err)
	}
}

func TestRequestCompact_ServerError(t *testing.T) {
	base, _ := newTestCompactService(t, map[string]any{"error": "boom"}, http.StatusInternalServerError)
	err := requestCompact(context.Background(), base, StaticToken("good"), "demo-project", "ctx-1")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want it to mention the server's message", err)
	}
}

func TestRequestCompact_Unauthorized(t *testing.T) {
	base, _ := newTestCompactService(t, map[string]any{"compacted": true}, http.StatusOK)
	err := requestCompact(context.Background(), base, StaticToken("wrong-token"), "demo-project", "ctx-1")
	if err == nil {
		t.Fatal("err = nil, want an error for a bad token")
	}
}

func TestRun_Compact(t *testing.T) {
	base, spy := newTestCompactService(t, map[string]any{"compacted": true}, http.StatusOK)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})
	var io capture
	code := Run(context.Background(),
		[]string{"compact", "--project", "demo-project", "--context-id", "ctx-1"}, env, &io)
	if code != 0 {
		t.Fatalf("code = %d, want 0\nstderr: %s", code, io.err.String())
	}
	if !strings.Contains(io.out.String(), "history compacted") {
		t.Errorf("stdout = %q, want it to report success", io.out.String())
	}
	if spy.ContextID != "ctx-1" || spy.ProjectName != "demo-project" {
		t.Fatalf("request body = %+v", spy)
	}
}

func TestRun_Compact_NothingToCompact(t *testing.T) {
	base, _ := newTestCompactService(t, map[string]any{"compacted": false}, http.StatusOK)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})
	var io capture
	code := Run(context.Background(),
		[]string{"compact", "--project", "demo-project", "--context-id", "ctx-1"}, env, &io)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (nothing to compact is not a failure)\nstderr: %s", code, io.err.String())
	}
	if !strings.Contains(io.out.String(), "nothing to compact") {
		t.Errorf("stdout = %q, want it to report nothing to compact", io.out.String())
	}
}

func TestRun_Compact_ServerErrorIsExitCode1(t *testing.T) {
	base, _ := newTestCompactService(t, map[string]any{"error": "boom"}, http.StatusInternalServerError)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})
	var io capture
	code := Run(context.Background(),
		[]string{"compact", "--project", "demo-project", "--context-id", "ctx-1"}, env, &io)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(io.err.String(), "boom") {
		t.Errorf("stderr = %q, want it to mention the server error", io.err.String())
	}
}

func TestRun_Compact_JSON(t *testing.T) {
	base, _ := newTestCompactService(t, map[string]any{"compacted": true}, http.StatusOK)
	env := envFn(map[string]string{"PATCH_URL": base, "PATCH_TOKEN": "good"})
	var io capture
	code := Run(context.Background(),
		[]string{"compact", "--project", "demo-project", "--context-id", "ctx-1", "--json"}, env, &io)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(io.out.String())), &body); err != nil {
		t.Fatalf("stdout is not JSON: %q (%v)", io.out.String(), err)
	}
	if compacted, _ := body["compacted"].(bool); !compacted {
		t.Fatalf("body = %+v, want compacted:true", body)
	}
}
