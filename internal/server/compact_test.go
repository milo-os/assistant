package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
)

// fakeCompactor is a scriptable [assistanta2a.Compactor]: it records the last
// request it saw and returns a fixed error (nil for success).
type fakeCompactor struct {
	err     error
	lastReq assistanta2a.CompactRequest
	calls   int
}

func (f *fakeCompactor) Compact(_ context.Context, req assistanta2a.CompactRequest) error {
	f.calls++
	f.lastReq = req
	return f.err
}

func postCompact(t *testing.T, srv *httptest.Server, token string, body map[string]any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/compact", bytes.NewReader(raw))
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

func TestCompactEndpoint_401WithoutToken(t *testing.T) {
	srv := newTestServerWithCompactor(t, &fakeCompactor{})
	res := postCompact(t, srv, "", map[string]any{"contextId": "c1", "projectName": project})
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

func TestCompactEndpoint_403UngrantedProject(t *testing.T) {
	srv := newTestServerWithCompactor(t, &fakeCompactor{})
	res := postCompact(t, srv, wrongToken, map[string]any{"contextId": "c1", "projectName": project})
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
}

func TestCompactEndpoint_400MissingContextID(t *testing.T) {
	srv := newTestServerWithCompactor(t, &fakeCompactor{})
	res := postCompact(t, srv, goodToken, map[string]any{"projectName": project})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestCompactEndpoint_400MissingProjectName(t *testing.T) {
	srv := newTestServerWithCompactor(t, &fakeCompactor{})
	res := postCompact(t, srv, goodToken, map[string]any{"contextId": "c1"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestCompactEndpoint_Success(t *testing.T) {
	compactor := &fakeCompactor{}
	srv := newTestServerWithCompactor(t, compactor)
	res := postCompact(t, srv, goodToken, map[string]any{"contextId": "c1", "projectName": project})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if compacted, _ := body["compacted"].(bool); !compacted {
		t.Fatalf("body = %+v, want compacted:true", body)
	}
	if compactor.calls != 1 {
		t.Fatalf("calls = %d, want 1", compactor.calls)
	}
	if compactor.lastReq.ProjectName != project || compactor.lastReq.ContextID != "c1" {
		t.Fatalf("lastReq = %+v", compactor.lastReq)
	}
}

func TestCompactEndpoint_NothingToCompactIsNotAnError(t *testing.T) {
	compactor := &fakeCompactor{err: assistanta2a.ErrNothingToCompact}
	srv := newTestServerWithCompactor(t, compactor)
	res := postCompact(t, srv, goodToken, map[string]any{"contextId": "c1", "projectName": project})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if compacted, _ := body["compacted"].(bool); compacted {
		t.Fatalf("body = %+v, want compacted:false", body)
	}
}

func TestCompactEndpoint_CompactorErrorIs500(t *testing.T) {
	compactor := &fakeCompactor{err: errors.New("boom")}
	srv := newTestServerWithCompactor(t, compactor)
	res := postCompact(t, srv, goodToken, map[string]any{"contextId": "c1", "projectName": project})
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
}

func TestCompactEndpoint_NilCompactorIs503(t *testing.T) {
	srv := newTestServerWithCompactor(t, nil)
	res := postCompact(t, srv, goodToken, map[string]any{"contextId": "c1", "projectName": project})
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}
