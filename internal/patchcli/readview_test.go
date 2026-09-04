package patchcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The whole point of the direct transport: the request carries datumctl's
// token and is addressed to the project the caller selected — not to whatever
// cluster kubectl's current context happens to name.
func TestReadViewDirectUsesDatumctlTokenAndProject(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"kind":"ConversationList","items":[]}`))
	}))
	defer srv.Close()

	view := ReadView{apiHost: srv.URL, token: func() (string, error) { return "tok-123", nil }}
	if !view.direct() {
		t.Fatal("want the direct transport when apiHost and token are set")
	}

	out, err := view.get(context.Background(), "demo-project", conversationsPath("demo-project"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(string(out), "ConversationList") {
		t.Fatalf("body = %s", out)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want the datumctl token", gotAuth)
	}
	// The project control-plane prefix is what scopes the request; without it
	// Milo has no project context and the authorizer denies the read.
	wantPath := "/apis/resourcemanager.miloapis.com/v1alpha1/projects/demo-project/control-plane" +
		"/apis/assistant.miloapis.com/v1alpha1/namespaces/demo-project/conversations"
	if gotPath != wantPath {
		t.Errorf("path  = %s\nwant  = %s", gotPath, wantPath)
	}
}

// A bare hostname is what datumctl injects (DATUM_API_HOST=api.datum.net), so
// it must not be parsed as a path.
func TestReadViewDirectDefaultsToHTTPS(t *testing.T) {
	view := ReadView{apiHost: "api.example.test", token: func() (string, error) { return "t", nil }}
	_, err := view.get(context.Background(), "p", "/apis/x")
	if err == nil {
		t.Fatal("want a transport error against a host that does not resolve")
	}
	if !strings.Contains(err.Error(), "https://api.example.test/") {
		t.Fatalf("err should show an https:// URL, got: %v", err)
	}
}

// Kubernetes puts the useful text in the status body's message; a bare "403
// Forbidden" hides which resource and which user.
func TestReadViewDirectSurfacesAPIMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","message":"conversations.assistant.miloapis.com is forbidden: User \"u\" cannot list","reason":"Forbidden"}`))
	}))
	defer srv.Close()

	view := ReadView{apiHost: srv.URL, token: func() (string, error) { return "t", nil }}
	_, err := view.get(context.Background(), "p", "/apis/x")
	if err == nil {
		t.Fatal("want an error for 403")
	}
	if !strings.Contains(err.Error(), "cannot list") {
		t.Fatalf("apiserver message was lost, got: %v", err)
	}
}

// --kubeconfig is an explicit request for a different identity. Preferring the
// datumctl token anyway would silently ignore the flag.
func TestReadViewKubeconfigForcesKubectl(t *testing.T) {
	view := ReadView{
		apiHost:    "https://api.example.test",
		token:      func() (string, error) { return "t", nil },
		kubeconfig: "/some/kubeconfig",
	}
	if view.direct() {
		t.Fatal("--kubeconfig must force the kubectl transport")
	}
}

// Without datumctl there is no token to send, so a host alone must not be
// mistaken for a usable direct transport — that is the standalone `patch`
// binary's situation.
func TestReadViewWithoutTokenFallsBackToKubectl(t *testing.T) {
	if (ReadView{apiHost: "https://api.example.test"}).direct() {
		t.Fatal("an API host with no token is not usable directly")
	}
	if (ReadView{token: func() (string, error) { return "t", nil }}).direct() {
		t.Fatal("a token with no API host is not usable directly")
	}
}
