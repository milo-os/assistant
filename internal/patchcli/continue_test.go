package patchcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if gotPath != "/v1/conversations/rename" {
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
	mux.HandleFunc("POST /v1/conversations/rename", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("POST /v1/conversations/rename", func(w http.ResponseWriter, _ *http.Request) {
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
