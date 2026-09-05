package mcptool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	neturl "net/url"
	"strings"
	"sync"
	"testing"
)

const testToken = "caller-secret-token"

// recordingServer fronts [newTestServer] with a recording reverse proxy, so a
// test sees the headers the session actually put on the wire.
func recordingServer(t *testing.T) (url string, headers func() []http.Header) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []http.Header
	)
	var calls []string
	target, err := neturl.Parse(newTestServer(t, &calls))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return append([]http.Header(nil), seen...)
	}
}

// Every request of a session — the initialize handshake and each tool call —
// carries the configured headers, which is what a provider MCP server that
// reads as the caller requires.
func TestHeadersAreSentOnEveryRequest(t *testing.T) {
	url, headers := recordingServer(t)

	sess, err := Connect(context.Background(), Options{
		Endpoint: url,
		Headers:  map[string]string{"Authorization": "Bearer " + testToken, "X-Datum-Project": "demo-project"},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if _, err := tools["pipeline_diagnose"].Execute(context.Background(), json.RawMessage(`{"id":"p-1"}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	seen := headers()
	if len(seen) < 2 {
		t.Fatalf("want at least the initialize and call requests, got %d", len(seen))
	}
	for i, h := range seen {
		if got := h.Get("Authorization"); got != "Bearer "+testToken {
			t.Fatalf("request %d Authorization = %q", i, got)
		}
		if got := h.Get("X-Datum-Project"); got != "demo-project" {
			t.Fatalf("request %d X-Datum-Project = %q", i, got)
		}
	}
}

// Without Headers the session sends no Authorization at all — the pre-existing
// behavior for a provider the operator has not sanctioned.
func TestNoHeadersWhenUnset(t *testing.T) {
	url, headers := recordingServer(t)

	sess, err := Connect(context.Background(), Options{Endpoint: url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()
	if _, err := sess.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}

	for i, h := range headers() {
		if got := h.Get("Authorization"); got != "" {
			t.Fatalf("request %d sent Authorization %q with no Headers configured", i, got)
		}
	}
}

// A round-tripper runs once per redirect hop, after the client's own
// cross-domain stripping, so the header injector must refuse any host that is
// not the endpoint's — otherwise a hostile redirect harvests the credential.
func TestHeaderRoundTripperOnlyStampsTheEndpointHost(t *testing.T) {
	var stamped []string
	rt := &headerRoundTripper{
		next: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			stamped = append(stamped, req.URL.Host+"="+req.Header.Get("Authorization"))
			return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
		}),
		host:    "gateway.example:80",
		headers: map[string]string{"Authorization": "Bearer " + testToken},
	}

	for _, target := range []string{
		"http://gateway.example:80/mcp",
		"http://GATEWAY.EXAMPLE:80/mcp", // host comparison is case-insensitive
		"http://evil.example/mcp",       // a redirect off the endpoint
		"http://gateway.example:8443/mcp",
	} {
		req, _ := http.NewRequest(http.MethodPost, target, nil)
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{
		"gateway.example:80=Bearer " + testToken,
		"GATEWAY.EXAMPLE:80=Bearer " + testToken,
		"evil.example=",
		"gateway.example:8443=",
	}
	for i, got := range stamped {
		if got != want[i] {
			t.Fatalf("hop %d = %q, want %q", i, got, want[i])
		}
	}
}

// A caller's token must not become part of an error the loop feeds back to the
// model.
func TestConnectErrorDoesNotCarryHeaders(t *testing.T) {
	_, err := Connect(context.Background(), Options{
		Endpoint: "http://127.0.0.1:1/mcp",
		Headers:  map[string]string{"Authorization": "Bearer " + testToken},
	})
	if err == nil {
		t.Fatal("expected a connect failure")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into the connect error: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
