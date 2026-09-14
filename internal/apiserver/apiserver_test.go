package apiserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	genericapiserver "k8s.io/apiserver/pkg/server"
	restclient "k8s.io/client-go/rest"
	basecompatibility "k8s.io/component-base/compatibility"

	"github.com/milo-os/assistant/internal/history"
	generatedopenapi "github.com/milo-os/assistant/pkg/generated/openapi"
)

// The conversations wire contract — the web UI and both CLIs are built
// against exactly these requests — is only partly the REST storage's: which
// verbs are routed, how a merge or JSON patch becomes an update, whether a
// field selector reaches List at all, and what a DELETE answers are all
// decided by the generic apiserver's handlers and this package's scheme. The
// storage unit tests cannot see any of that, so these drive the assembled
// handler chain over HTTP, with an in-memory store and no auth.
func newTestAPI(t *testing.T) (*httptest.Server, *history.MemoryStore) {
	t.Helper()
	store := history.NewMemoryStore()
	ctx := context.Background()
	for _, id := range []string{"ctx-a", "ctx-b"} {
		if err := store.Append(ctx, "demo", id, history.Turn{UserText: "about " + id, AssistantText: "ok"}); err != nil {
			t.Fatal(err)
		}
	}

	gen := genericapiserver.NewRecommendedConfig(Codecs)
	gen.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("1.36", "", "")
	gen.ExternalAddress = "127.0.0.1:443"
	gen.LoopbackClientConfig = &restclient.Config{}
	namer := openapinamer.NewDefinitionNamer(Scheme)
	gen.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(generatedopenapi.GetOpenAPIDefinitions, namer)
	gen.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(generatedopenapi.GetOpenAPIDefinitions, namer)
	cfg := &Config{GenericConfig: gen, ExtraConfig: ExtraConfig{Conversations: store}}
	server, err := cfg.Complete().New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(server.GenericAPIServer.Handler)
	t.Cleanup(srv.Close)
	return srv, store
}

const conversationsURL = "/apis/assistant.miloapis.com/v1alpha1/namespaces/demo/conversations"

func do(t *testing.T, srv *httptest.Server, method, path, contentType, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"raw": string(raw)}
	}
	return resp.StatusCode, out
}

func names(list map[string]any) []string {
	var out []string
	items, _ := list["items"].([]any)
	for _, it := range items {
		meta, _ := it.(map[string]any)["metadata"].(map[string]any)
		out = append(out, meta["name"].(string))
	}
	return out
}

func field(obj map[string]any, path ...string) any {
	var cur any = obj
	for _, p := range path {
		m, _ := cur.(map[string]any)
		cur = m[p]
	}
	return cur
}

func TestWireArchiveUnarchiveAndList(t *testing.T) {
	srv, _ := newTestAPI(t)

	code, obj := do(t, srv, http.MethodPatch, conversationsURL+"/ctx-a", "application/merge-patch+json", `{"spec":{"archived":true}}`)
	if code != http.StatusOK || field(obj, "spec", "archived") != true || field(obj, "status", "archivedAt") == nil {
		t.Fatalf("merge patch = %d %v", code, obj)
	}
	if field(obj, "metadata", "uid") == nil {
		t.Fatalf("conversation carries no uid: %v", obj)
	}
	stale := `{"metadata":{"uid":"00000000-0000-0000-0000-000000000000"},"spec":{"archived":false}}`
	if code, body := do(t, srv, http.MethodPatch, conversationsURL+"/ctx-a", "application/merge-patch+json", stale); code != http.StatusConflict {
		t.Fatalf("patch with a stale uid = %d %v, want 409", code, body)
	}
	archivedAt := field(obj, "status", "archivedAt")

	if code, list := do(t, srv, http.MethodGet, conversationsURL, "", ""); code != http.StatusOK || strings.Join(names(list), ",") != "ctx-b" {
		t.Fatalf("default list = %d %v, want only ctx-b", code, names(list))
	}
	sel := "?fieldSelector=" + url.QueryEscape("spec.archived=true")
	if code, list := do(t, srv, http.MethodGet, conversationsURL+sel, "", ""); code != http.StatusOK || strings.Join(names(list), ",") != "ctx-a" {
		t.Fatalf("archived list = %d %v, want only ctx-a", code, names(list))
	}
	sel = "?fieldSelector=" + url.QueryEscape("spec.archived=false") + "&limit=5"
	if code, list := do(t, srv, http.MethodGet, conversationsURL+sel, "", ""); code != http.StatusOK || strings.Join(names(list), ",") != "ctx-b" {
		t.Fatalf("explicit default list = %d %v", code, names(list))
	}
	for _, bad := range []string{"metadata.name=ctx-a", "spec.archived=maybe", "status.title=x"} {
		if code, body := do(t, srv, http.MethodGet, conversationsURL+"?fieldSelector="+url.QueryEscape(bad), "", ""); code != http.StatusBadRequest {
			t.Errorf("fieldSelector %s = %d %v, want 400", bad, code, body)
		}
	}

	// Archived conversations stay gettable, and re-archiving keeps the time.
	code, obj = do(t, srv, http.MethodPatch, conversationsURL+"/ctx-a", "application/merge-patch+json", `{"spec":{"archived":true}}`)
	if code != http.StatusOK || field(obj, "status", "archivedAt") != archivedAt {
		t.Fatalf("re-archive = %d archivedAt %v, want the original %v", code, field(obj, "status", "archivedAt"), archivedAt)
	}

	code, obj = do(t, srv, http.MethodPatch, conversationsURL+"/ctx-a", "application/json-patch+json", `[{"op":"replace","path":"/spec/archived","value":false}]`)
	if code != http.StatusOK || field(obj, "spec", "archived") != nil || field(obj, "status", "archivedAt") != nil {
		t.Fatalf("json patch unarchive = %d %v", code, obj)
	}
	if code, list := do(t, srv, http.MethodGet, conversationsURL, "", ""); code != http.StatusOK || len(names(list)) != 2 {
		t.Fatalf("list after unarchive = %d %v", code, names(list))
	}
}

// PUT is what `kubectl replace --raw` sends: a whole object, possibly a stale
// read. Only spec.archived may land.
func TestWirePutAppliesOnlySpecArchived(t *testing.T) {
	srv, _ := newTestAPI(t)
	_, current := do(t, srv, http.MethodGet, conversationsURL+"/ctx-b", "", "")
	current["spec"] = map[string]any{"archived": true}
	current["status"].(map[string]any)["title"] = "forged"
	current["metadata"].(map[string]any)["labels"] = map[string]any{"forged": "yes"}
	body, _ := json.Marshal(current)

	code, obj := do(t, srv, http.MethodPut, conversationsURL+"/ctx-b", "application/json", string(body))
	if code != http.StatusOK || field(obj, "spec", "archived") != true {
		t.Fatalf("put = %d %v", code, obj)
	}
	if field(obj, "status", "title") != "about ctx-b" || field(obj, "metadata", "labels") != nil {
		t.Fatalf("put leaked non-spec fields: %v", obj)
	}

	if code, _ := do(t, srv, http.MethodPut, conversationsURL+"/missing", "application/json",
		`{"apiVersion":"assistant.miloapis.com/v1alpha1","kind":"Conversation","metadata":{"name":"missing"},"spec":{"archived":true}}`); code != http.StatusNotFound {
		t.Fatalf("put to a missing conversation = %d, want 404 (no create on update)", code)
	}
	if code, _ := do(t, srv, http.MethodPatch, conversationsURL+"/missing", "application/merge-patch+json", `{"spec":{"archived":true}}`); code != http.StatusNotFound {
		t.Fatalf("patch of a missing conversation = %d, want 404", code)
	}
}

func TestWireDelete(t *testing.T) {
	srv, store := newTestAPI(t)
	code, obj := do(t, srv, http.MethodDelete, conversationsURL+"/ctx-a", "", "")
	if code != http.StatusOK || field(obj, "kind") != "Conversation" || field(obj, "metadata", "name") != "ctx-a" {
		t.Fatalf("delete = %d %v, want 200 with the deleted conversation", code, obj)
	}
	if code, _ := do(t, srv, http.MethodGet, conversationsURL+"/ctx-a", "", ""); code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", code)
	}
	if turns, _ := store.Turns(context.Background(), "demo", "ctx-a"); turns != nil {
		t.Fatalf("messages survived the delete: %v", turns)
	}
	if code, _ := do(t, srv, http.MethodDelete, conversationsURL+"/ctx-a", "", ""); code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", code)
	}
	// No deletecollection: one request must never be able to wipe a project.
	if code, _ := do(t, srv, http.MethodDelete, conversationsURL, "", ""); code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
		t.Fatalf("delete collection = %d, want it unsupported", code)
	}
	if code, list := do(t, srv, http.MethodGet, conversationsURL, "", ""); code != http.StatusOK || strings.Join(names(list), ",") != "ctx-b" {
		t.Fatalf("list after delete = %d %v", code, names(list))
	}
}

// Continuing an archived conversation — any path that appends a turn — puts
// it back in the default list.
func TestWireAppendUnarchives(t *testing.T) {
	srv, store := newTestAPI(t)
	if code, _ := do(t, srv, http.MethodPatch, conversationsURL+"/ctx-a", "application/merge-patch+json", `{"spec":{"archived":true}}`); code != http.StatusOK {
		t.Fatalf("archive = %d", code)
	}
	if err := store.Append(context.Background(), "demo", "ctx-a", history.Turn{UserText: "back", AssistantText: "hi"}); err != nil {
		t.Fatal(err)
	}
	if code, obj := do(t, srv, http.MethodGet, conversationsURL+"/ctx-a", "", ""); code != http.StatusOK || field(obj, "spec", "archived") != nil {
		t.Fatalf("get after append = %d %v, want unarchived", code, obj)
	}
}
