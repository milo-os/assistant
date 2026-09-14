package patchcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// conversationListView serves a fixed conversation listing over the direct
// (Milo) read-view transport, so -c/--continue can be exercised without
// kubectl or a cluster.
func conversationListView(t *testing.T, body string) ReadView {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return ReadView{apiHost: srv.URL, token: func() (string, error) { return "tok", nil }}
}

// listing renders conversations as the apiserver would, deliberately NOT in
// activity order — "most recent" must come from the timestamps, not the row
// order the server happened to send.
func listing(rows ...map[string]any) string {
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": r["name"]},
			"status":   map[string]any{"lastActiveAt": r["lastActiveAt"]},
		})
	}
	b, _ := json.Marshal(map[string]any{"kind": "ConversationList", "items": items})
	return string(b)
}

func TestLatestConversationPicksTheNewestActivity(t *testing.T) {
	view := conversationListView(t, listing(
		map[string]any{"name": "older", "lastActiveAt": "2026-07-17T10:00:00Z"},
		map[string]any{"name": "newest", "lastActiveAt": "2026-07-18T09:30:00Z"},
		map[string]any{"name": "middle", "lastActiveAt": "2026-07-17T22:00:00Z"},
	))
	got, err := latestConversation(context.Background(), view, "demo")
	if err != nil {
		t.Fatalf("latestConversation: %v", err)
	}
	if got != "newest" {
		t.Fatalf("got %q, want newest", got)
	}
}

func TestLatestConversationEmptyProject(t *testing.T) {
	view := conversationListView(t, listing())
	got, err := latestConversation(context.Background(), view, "demo")
	if err != nil || got != "" {
		t.Fatalf("got %q, %v; want empty, nil", got, err)
	}
}

// Nothing to continue is not a failure: say so and start fresh, rather than
// refusing to open a chat at all.
func TestContinueContextIDReportsAnEmptyProject(t *testing.T) {
	view := conversationListView(t, listing())
	inv := Invocation{Project: "demo", APIHost: view.apiHost, Token: view.token}
	var io capture
	if got := continueContextID(context.Background(), inv, &io); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if !strings.Contains(io.err.String(), "no conversations in project demo") {
		t.Fatalf("stderr = %q, want it to say the project has none", io.err.String())
	}
	if io.out.String() != "" {
		t.Fatalf("stdout = %q, want the message on stderr only", io.out.String())
	}
}

// A read-view failure is reported the same way, so a broken listing degrades
// to a fresh conversation instead of an error the user cannot act on mid-chat.
func TestContinueContextIDReportsALookupFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"conversations is forbidden"}`))
	}))
	defer srv.Close()
	inv := Invocation{Project: "demo", APIHost: srv.URL, Token: func() (string, error) { return "tok", nil }}
	var io capture
	if got := continueContextID(context.Background(), inv, &io); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if !strings.Contains(io.err.String(), "conversations is forbidden") {
		t.Fatalf("stderr = %q, want the apiserver's own message", io.err.String())
	}
}

func TestRequestRenamePostsTheConversationAndName(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"renamed":true,"name":"dfw quota escalation"}`))
	}))
	defer srv.Close()

	err := requestRename(context.Background(), srv.URL, StaticToken("tok-123"),
		"demo", "ctx-1", "dfw quota escalation")
	if err != nil {
		t.Fatalf("requestRename: %v", err)
	}
	if gotPath != "/v1alpha1/conversations/rename" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	want := map[string]string{"contextId": "ctx-1", "projectName": "demo", "name": "dfw quota escalation"}
	for k, v := range want {
		if gotBody[k] != v {
			t.Errorf("body[%q] = %q, want %q", k, gotBody[k], v)
		}
	}
}

// `conversations rename` is the one subcommand of `conversations` that talks
// to the service, so it must resolve PATCH_URL/PATCH_TOKEN like `compact` does
// rather than falling into the read view's kubectl path.
func TestRun_ConversationsRename(t *testing.T) {
	var gotBody map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1alpha1/conversations/rename", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"renamed":true,"name":"dfw quota escalation"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	env := envFn(map[string]string{"PATCH_URL": srv.URL, "PATCH_TOKEN": "good"})
	var io capture
	code := Run(context.Background(),
		[]string{"conversations", "rename", "ctx-1", "dfw quota escalation", "--project", "demo"}, env, &io)
	if code != 0 {
		t.Fatalf("code = %d, want 0\nstderr: %s", code, io.err.String())
	}
	if gotBody["contextId"] != "ctx-1" || gotBody["name"] != "dfw quota escalation" {
		t.Fatalf("request body = %+v", gotBody)
	}
	if !strings.Contains(io.out.String(), "renamed ctx-1 to dfw quota escalation") {
		t.Errorf("stdout = %q", io.out.String())
	}
}

func TestRun_ConversationsRename_ServerErrorIsExitCode1(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1alpha1/conversations/rename", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Conversation not found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	env := envFn(map[string]string{"PATCH_URL": srv.URL, "PATCH_TOKEN": "good"})
	var io capture
	code := Run(context.Background(),
		[]string{"conversations", "rename", "nope", "a name", "--project", "demo"}, env, &io)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(io.err.String(), "Conversation not found") {
		t.Errorf("stderr = %q", io.err.String())
	}
}

// The service's own message is what the user needs (which conversation, why),
// so it must survive the round trip rather than becoming a bare status line.
func TestRequestRenameSurfacesTheServiceMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Conversation not found"}`))
	}))
	defer srv.Close()

	err := requestRename(context.Background(), srv.URL, StaticToken("t"), "demo", "nope", "n")
	if err == nil || !strings.Contains(err.Error(), "Conversation not found") {
		t.Fatalf("err = %v, want the service's message", err)
	}
}

// -c/--continue must never land in an archived conversation. The apiserver's
// default listing already leaves them out; this pins that the client asks for
// exactly that listing (no field selector) rather than the archive, so a
// recently active archived conversation cannot win on timestamp.
func TestLatestConversationNeverPicksArchived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fieldSelector") != "" {
			_, _ = w.Write([]byte(listing(map[string]any{"name": "archived-newest", "lastActiveAt": "2026-07-19T00:00:00Z"})))
			return
		}
		_, _ = w.Write([]byte(listing(map[string]any{"name": "active", "lastActiveAt": "2026-07-17T00:00:00Z"})))
	}))
	defer srv.Close()
	view := ReadView{apiHost: srv.URL, token: func() (string, error) { return "tok", nil }}
	got, err := latestConversation(context.Background(), view, "demo")
	if err != nil || got != "active" {
		t.Fatalf("got %q, %v; want the newest unarchived conversation", got, err)
	}
}

// conversationAPI is a fake of the aggregated API's conversation endpoints over
// the direct transport, recording what each request asked for.
type conversationAPI struct {
	srv     *httptest.Server
	methods []string
	query   string
	patch   string
	missing bool
}

func newConversationAPI(t *testing.T) *conversationAPI {
	t.Helper()
	api := &conversationAPI{}
	api.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.methods = append(api.methods, r.Method)
		if api.missing {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"conversations.assistant.miloapis.com \"ctx-1\" not found"}`))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/conversations"):
			api.query = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"kind":"ConversationList","items":[]}`))
		case r.Method == http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			api.patch = string(b)
			_, _ = w.Write([]byte(`{"kind":"Conversation","metadata":{"name":"ctx-1"}}`))
		default:
			_, _ = w.Write([]byte(`{"kind":"Conversation","metadata":{"name":"ctx-1"},"status":{"title":"why is p-1 down?"}}`))
		}
	}))
	t.Cleanup(api.srv.Close)
	return api
}

func (api *conversationAPI) invocation(kind Kind) Invocation {
	return Invocation{Kind: kind, Project: "demo", ContextID: "ctx-1", APIHost: api.srv.URL, Token: StaticToken("tok")}
}

func (api *conversationAPI) sent(method string) bool {
	return slices.Contains(api.methods, method)
}

// answering is an [Io] with a terminal's answers queued on it.
type answering struct {
	capture
	answers []string
}

func (a *answering) ReadLine() (string, bool) {
	if len(a.answers) == 0 {
		return "", false
	}
	line := a.answers[0]
	a.answers = a.answers[1:]
	return line, true
}

func TestConversationsListArchivedSendsTheFieldSelector(t *testing.T) {
	api := newConversationAPI(t)
	inv := api.invocation(KindConvList)
	inv.Archived = true
	var out capture
	if code := inv.Execute(context.Background(), &out); code != 0 {
		t.Fatalf("code = %d, stderr %s", code, out.err.String())
	}
	if api.query != "fieldSelector=spec.archived%3Dtrue" {
		t.Errorf("query = %q, want the spec.archived=true selector", api.query)
	}
	if !strings.Contains(out.err.String(), "no archived conversations in project demo") {
		t.Errorf("stderr = %q", out.err.String())
	}

	inv.Archived = false
	if code := inv.Execute(context.Background(), &out); code != 0 || api.query != "" {
		t.Fatalf("default list code=%d query=%q, want no selector", code, api.query)
	}
}

func TestConversationsArchiveAndUnarchive(t *testing.T) {
	api := newConversationAPI(t)
	var out capture
	if code := api.invocation(KindConvArchive).Execute(context.Background(), &out); code != 0 {
		t.Fatalf("archive code = %d, stderr %s", code, out.err.String())
	}
	if api.patch != `{"spec":{"archived":true}}` {
		t.Errorf("patch = %s", api.patch)
	}
	if out.out.String() != "archived ctx-1\n" {
		t.Errorf("stdout = %q", out.out.String())
	}
	if !strings.Contains(out.err.String(), "conversations list --archived --project demo") {
		t.Errorf("archive should say how to find it again, stderr = %q", out.err.String())
	}

	inv := api.invocation(KindConvUnarchive)
	inv.JSON = true
	var jsonOut capture
	if code := inv.Execute(context.Background(), &jsonOut); code != 0 {
		t.Fatalf("unarchive code = %d", code)
	}
	if api.patch != `{"spec":{"archived":false}}` {
		t.Errorf("patch = %s", api.patch)
	}
	if got := strings.TrimSpace(jsonOut.out.String()); got != `{"contextId":"ctx-1","unarchived":true}` {
		t.Errorf("json = %s", got)
	}
	if jsonOut.err.String() != "" {
		t.Errorf("-o json should keep stderr quiet, got %q", jsonOut.err.String())
	}
}

func TestConversationsArchiveNotFoundIsExitCode1(t *testing.T) {
	api := newConversationAPI(t)
	api.missing = true
	inv := api.invocation(KindConvArchive)
	inv.JSON = true
	var out capture
	if code := inv.Execute(context.Background(), &out); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out.out.String()), &body); err != nil || body["archived"] != false ||
		!strings.Contains(fmt.Sprint(body["error"]), "not found") {
		t.Fatalf("json = %s (%v)", out.out.String(), err)
	}
}

// With nobody at a terminal to ask, delete refuses — and says how to proceed —
// rather than treating a pipe as consent. Nothing is sent.
func TestConversationsDeleteWithoutATerminalNeedsYes(t *testing.T) {
	api := newConversationAPI(t)
	io := &answering{answers: []string{"y"}}
	if code := api.invocation(KindConvDelete).Execute(context.Background(), io); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(io.err.String(), "pass --yes") {
		t.Errorf("stderr = %q, want it to say to pass --yes", io.err.String())
	}
	if len(api.methods) != 0 {
		t.Errorf("requests sent without confirmation: %v", api.methods)
	}
}

func TestConversationsDeleteAsksOnATerminal(t *testing.T) {
	api := newConversationAPI(t)
	inv := api.invocation(KindConvDelete)
	inv.StdinTerminal = true

	declined := &answering{answers: []string{""}}
	if code := inv.Execute(context.Background(), declined); code != 1 {
		t.Fatalf("declined code = %d, want 1", code)
	}
	if want := "delete conversation why is p-1 down? (ctx-1)? this cannot be undone [y/N] "; !strings.HasPrefix(declined.err.String(), want) {
		t.Errorf("prompt = %q, want %q", declined.err.String(), want)
	}
	if api.sent(http.MethodDelete) {
		t.Fatal("the default answer deleted the conversation")
	}

	accepted := &answering{answers: []string{"y"}}
	if code := inv.Execute(context.Background(), accepted); code != 0 {
		t.Fatalf("accepted code = %d, stderr %s", code, accepted.err.String())
	}
	if !api.sent(http.MethodDelete) || accepted.out.String() != "deleted ctx-1\n" {
		t.Fatalf("methods=%v stdout=%q", api.methods, accepted.out.String())
	}
}

// --yes is the scripted form: no lookup, no prompt, just the delete.
func TestConversationsDeleteYesSkipsThePrompt(t *testing.T) {
	api := newConversationAPI(t)
	inv := api.invocation(KindConvDelete)
	inv.Yes = true
	inv.JSON = true
	var out capture
	if code := inv.Execute(context.Background(), &out); code != 0 {
		t.Fatalf("code = %d, stderr %s", code, out.err.String())
	}
	if len(api.methods) != 1 || api.methods[0] != http.MethodDelete {
		t.Errorf("methods = %v, want a single DELETE", api.methods)
	}
	if got := strings.TrimSpace(out.out.String()); got != `{"contextId":"ctx-1","deleted":true}` {
		t.Errorf("json = %s", got)
	}
}

// A mistyped id is found out before anyone is asked to confirm deleting it.
func TestConversationsDeleteUnknownIsReportedBeforeAsking(t *testing.T) {
	api := newConversationAPI(t)
	api.missing = true
	inv := api.invocation(KindConvDelete)
	inv.StdinTerminal = true
	io := &answering{answers: []string{"y"}}
	if code := inv.Execute(context.Background(), io); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if strings.Contains(io.err.String(), "[y/N]") || !strings.Contains(io.err.String(), "not found") {
		t.Errorf("stderr = %q, want the not-found and no prompt", io.err.String())
	}
}

// The lifecycle writes go to the aggregated API, not the assistant service, so
// the standalone CLI must not demand PATCH_URL for them.
func TestRun_ConversationWritesNeedNoServiceURL(t *testing.T) {
	for _, k := range []Kind{KindConvArchive, KindConvUnarchive, KindConvDelete} {
		if (Invocation{Kind: k}).needsService() {
			t.Errorf("kind %d classified as needing the service", k)
		}
	}
	if !(Invocation{Kind: KindConvRename}).needsService() {
		t.Error("rename still goes to the service")
	}

	calls := stubKubectl(t, func(args []string, stdin []byte) ([]byte, error) {
		if args[0] == "get" {
			return []byte(`{"kind":"Conversation","metadata":{"name":"ctx-1"}}`), nil
		}
		return stdin, nil
	})
	var out capture
	code := Run(context.Background(), []string{"conversations", "archive", "ctx-1", "--project", "demo"}, envFn(nil), &out)
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, out.err.String())
	}
	if len(*calls) != 2 {
		t.Fatalf("kubectl calls = %+v", *calls)
	}
}
