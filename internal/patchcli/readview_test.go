package patchcli

import (
	"context"
	"encoding/json"
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

// Archive is a merge patch naming only spec.archived, so nothing else on the
// object can be disturbed, sent to the conversation itself under the project's
// control plane with the datumctl token.
func TestReadViewArchiveSendsAMergePatch(t *testing.T) {
	var gotMethod, gotPath, gotType, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotType, gotAuth = r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"kind":"Conversation","metadata":{"name":"ctx-1"},"spec":{"archived":true},"status":{"archivedAt":"2026-07-18T09:00:00Z"}}`))
	}))
	defer srv.Close()

	view := ReadView{apiHost: srv.URL, token: func() (string, error) { return "tok-123", nil }}
	conv, err := view.setConversationArchived(context.Background(), "demo-project", "ctx-1", true)
	if err != nil {
		t.Fatalf("setConversationArchived: %v", err)
	}
	if gotMethod != http.MethodPatch || gotType != "application/merge-patch+json" {
		t.Errorf("request = %s %s, want PATCH application/merge-patch+json", gotMethod, gotType)
	}
	wantPath := "/apis/resourcemanager.miloapis.com/v1alpha1/projects/demo-project/control-plane" +
		"/apis/assistant.miloapis.com/v1alpha1/namespaces/demo-project/conversations/ctx-1"
	if gotPath != wantPath {
		t.Errorf("path  = %s\nwant  = %s", gotPath, wantPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	spec, _ := gotBody["spec"].(map[string]any)
	if len(gotBody) != 1 || len(spec) != 1 || spec["archived"] != true {
		t.Errorf("body = %v, want exactly {\"spec\":{\"archived\":true}}", gotBody)
	}
	if !conv.Spec.Archived || conv.Status.ArchivedAt == nil {
		t.Errorf("returned conversation = %+v, want the server's archived object", conv)
	}

	if _, err := view.setConversationArchived(context.Background(), "demo-project", "ctx-1", false); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if spec, _ := gotBody["spec"].(map[string]any); spec["archived"] != false {
		t.Errorf("unarchive body = %v, want spec.archived false", gotBody)
	}
}

// Delete is a DELETE of the conversation; 202 is the Kubernetes way of saying
// "under way" and must not read as a failure, while an error status carries
// the apiserver's own message.
func TestReadViewDeleteConversation(t *testing.T) {
	status := http.StatusAccepted
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(status)
		if status == http.StatusNotFound {
			_, _ = w.Write([]byte(`{"kind":"Status","message":"conversations.assistant.miloapis.com \"ctx-1\" not found","reason":"NotFound"}`))
		}
	}))
	defer srv.Close()

	view := ReadView{apiHost: srv.URL, token: func() (string, error) { return "t", nil }}
	if err := view.deleteConversation(context.Background(), "p", "ctx-1"); err != nil {
		t.Fatalf("202: %v", err)
	}
	if gotMethod != http.MethodDelete || !strings.HasSuffix(gotPath, "/namespaces/p/conversations/ctx-1") {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	status = http.StatusNotFound
	err := view.deleteConversation(context.Background(), "p", "ctx-1")
	if err == nil || !strings.Contains(err.Error(), `"ctx-1" not found`) {
		t.Fatalf("404 err = %v, want the apiserver's message", err)
	}
}

// stubKubectl replaces the kubectl runner for one test, recording every
// invocation's args and stdin.
type kubectlCall struct {
	args  []string
	stdin []byte
}

func stubKubectl(t *testing.T, respond func(args []string, stdin []byte) ([]byte, error)) *[]kubectlCall {
	t.Helper()
	var calls []kubectlCall
	orig := kubectlRun
	kubectlRun = func(_ context.Context, kubeconfig string, stdin []byte, args ...string) ([]byte, error) {
		full := args
		if kubeconfig != "" {
			full = append([]string{"--kubeconfig", kubeconfig}, args...)
		}
		calls = append(calls, kubectlCall{args: full, stdin: stdin})
		return respond(args, stdin)
	}
	t.Cleanup(func() { kubectlRun = orig })
	return &calls
}

// kubectl has no `patch --raw`, so the fallback reads the object and replaces
// it whole. Unknown fields must survive the round trip, and the only change in
// the body is spec.archived.
func TestReadViewKubectlArchiveReplacesTheObject(t *testing.T) {
	path := "/apis/assistant.miloapis.com/v1alpha1/namespaces/p/conversations/ctx-1"
	calls := stubKubectl(t, func(args []string, stdin []byte) ([]byte, error) {
		if args[0] == "get" {
			return []byte(`{"kind":"Conversation","metadata":{"name":"ctx-1"},"status":{"title":"t"},"futureField":{"x":1}}`), nil
		}
		return stdin, nil
	})

	view := ReadView{kubeconfig: "/kc"}
	conv, err := view.setConversationArchived(context.Background(), "p", "ctx-1", true)
	if err != nil {
		t.Fatalf("setConversationArchived: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("kubectl calls = %+v, want get then replace", *calls)
	}
	if got := strings.Join((*calls)[0].args, " "); got != "--kubeconfig /kc get --raw "+path {
		t.Errorf("get = %q", got)
	}
	if got := strings.Join((*calls)[1].args, " "); got != "--kubeconfig /kc replace --raw "+path+" -f -" {
		t.Errorf("replace = %q", got)
	}
	var sent map[string]any
	if err := json.Unmarshal((*calls)[1].stdin, &sent); err != nil {
		t.Fatalf("replace stdin is not JSON: %v", err)
	}
	if spec, _ := sent["spec"].(map[string]any); spec["archived"] != true {
		t.Errorf("replace body spec = %v, want archived true", sent["spec"])
	}
	if _, ok := sent["futureField"]; !ok {
		t.Errorf("replace body dropped a field it did not know: %v", sent)
	}
	if !conv.Spec.Archived {
		t.Errorf("returned conversation = %+v", conv)
	}
}

func TestReadViewKubectlDelete(t *testing.T) {
	calls := stubKubectl(t, func([]string, []byte) ([]byte, error) { return []byte(`{}`), nil })
	if err := (ReadView{}).deleteConversation(context.Background(), "p", "ctx-1"); err != nil {
		t.Fatalf("deleteConversation: %v", err)
	}
	want := "delete --raw /apis/assistant.miloapis.com/v1alpha1/namespaces/p/conversations/ctx-1"
	if len(*calls) != 1 || strings.Join((*calls)[0].args, " ") != want {
		t.Fatalf("kubectl calls = %+v, want %q", *calls, want)
	}
}

// The id lands in the path an irreversible DELETE is sent to, so it must stay
// inside its own segment.
func TestConversationPathEscapesTheID(t *testing.T) {
	got := conversationPath("p", "../../other")
	if strings.Contains(got, "/../") || !strings.HasSuffix(got, "/conversations/..%2F..%2Fother") {
		t.Fatalf("conversationPath = %q, want the id escaped into one segment", got)
	}
}
