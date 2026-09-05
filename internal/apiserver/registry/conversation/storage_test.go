package conversation

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/tenant"
	"github.com/milo-os/assistant/pkg/apis/assistant"
)

// fakeReader is an in-test history.Reader that records the project every call
// was scoped to, so tests can assert tenancy filtering.
type fakeReader struct {
	convs        map[string][]history.Conversation // project -> conversations
	msgs         map[string][]history.Message      // project|id -> messages
	lastProject  string
	forceListErr error
}

func (f *fakeReader) ListConversations(_ context.Context, project string, _ int) ([]history.Conversation, error) {
	f.lastProject = project
	if f.forceListErr != nil {
		return nil, f.forceListErr
	}
	return f.convs[project], nil
}

func (f *fakeReader) GetConversation(_ context.Context, project, id string) (history.Conversation, error) {
	f.lastProject = project
	for _, c := range f.convs[project] {
		if c.ContextID == id {
			return c, nil
		}
	}
	return history.Conversation{}, history.ErrConversationNotFound
}

func (f *fakeReader) Messages(_ context.Context, project, id string) ([]history.Message, error) {
	f.lastProject = project
	return f.msgs[project+"|"+id], nil
}

func nsCtx(ns string) context.Context {
	return request.WithNamespace(context.Background(), ns)
}

func TestConversationGet(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	reader := &fakeReader{convs: map[string][]history.Conversation{
		"demo": {{ProjectName: "demo", ContextID: "ctx-1", CreatedAt: now, LastActiveAt: now.Add(time.Hour), TurnCount: 3, Title: "why is p-1 down?"}},
	}}
	rest := NewConversationREST(reader)

	obj, err := rest.Get(nsCtx("demo"), "ctx-1", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c := obj.(*assistant.Conversation)
	if c.Name != "ctx-1" || c.Namespace != "demo" {
		t.Errorf("meta = %q/%q, want demo/ctx-1", c.Namespace, c.Name)
	}
	if c.Status.MessageCount != 3 {
		t.Errorf("MessageCount = %d, want 3", c.Status.MessageCount)
	}
	if c.Status.Title != "why is p-1 down?" {
		t.Errorf("Title = %q, want the store's title", c.Status.Title)
	}
	if !c.CreationTimestamp.Time.Equal(now) {
		t.Errorf("CreationTimestamp = %v, want %v", c.CreationTimestamp.Time, now)
	}
	if reader.lastProject != "demo" {
		t.Errorf("query scoped to %q, want demo", reader.lastProject)
	}
}

// The name is a separate field from the title, not a replacement for it: a
// client that shows the name still needs the derived title as its fallback,
// and a listing that dropped the title on rename could never fall back.
func TestConversationNameAndTitleAreBothSurfaced(t *testing.T) {
	reader := &fakeReader{convs: map[string][]history.Conversation{
		"demo": {
			{ProjectName: "demo", ContextID: "named", Title: "why is p-1 down?", Name: "dfw quota escalation"},
			{ProjectName: "demo", ContextID: "unnamed", Title: "why is p-2 down?"},
		},
	}}
	rest := NewConversationREST(reader)

	obj, err := rest.Get(nsCtx("demo"), "named", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c := obj.(*assistant.Conversation)
	if c.Status.Name != "dfw quota escalation" {
		t.Errorf("Name = %q, want the store's name", c.Status.Name)
	}
	if c.Status.Title != "why is p-1 down?" {
		t.Errorf("Title = %q, want it kept alongside the name", c.Status.Title)
	}

	listObj, err := rest.List(nsCtx("demo"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	items := listObj.(*assistant.ConversationList).Items
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Status.Name != "dfw quota escalation" {
		t.Errorf("list[0].Name = %q", items[0].Status.Name)
	}
	if items[1].Status.Name != "" {
		t.Errorf("list[1].Name = %q, want empty for a conversation never named", items[1].Status.Name)
	}
}

func TestConversationGetNotFound(t *testing.T) {
	rest := NewConversationREST(&fakeReader{convs: map[string][]history.Conversation{}})
	_, err := rest.Get(nsCtx("demo"), "missing", &metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestConversationGetNullByteNameIsBadRequest(t *testing.T) {
	rest := NewConversationREST(&fakeReader{convs: map[string][]history.Conversation{}})
	_, err := rest.Get(nsCtx("demo"), "foo\x00bar", &metav1.GetOptions{})
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest", err)
	}
}

func TestConversationListScopedToNamespace(t *testing.T) {
	reader := &fakeReader{convs: map[string][]history.Conversation{
		"demo":  {{ProjectName: "demo", ContextID: "a"}, {ProjectName: "demo", ContextID: "b"}},
		"other": {{ProjectName: "other", ContextID: "z"}},
	}}
	rest := NewConversationREST(reader)

	obj, err := rest.List(nsCtx("demo"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	list := obj.(*assistant.ConversationList)
	if len(list.Items) != 2 {
		t.Fatalf("got %d items, want 2 (only demo's)", len(list.Items))
	}
	if reader.lastProject != "demo" {
		t.Errorf("list scoped to %q, want demo", reader.lastProject)
	}
}

func TestListMissingNamespaceIsBadRequest(t *testing.T) {
	rest := NewConversationREST(&fakeReader{})
	_, err := rest.List(context.Background(), &metainternalversion.ListOptions{})
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest", err)
	}
}

// A project-scoped milo identity may not read a different project's namespace.
func TestProjectIdentityMismatchIsForbidden(t *testing.T) {
	ctx := request.WithUser(nsCtx("demo"), &user.DefaultInfo{
		Name: "alice",
		Extra: map[string][]string{
			tenant.ExtraParentType: {"Project"},
			tenant.ExtraParentName: {"other"},
		},
	})
	rest := NewConversationREST(&fakeReader{})
	_, err := rest.List(ctx, &metainternalversion.ListOptions{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want Forbidden", err)
	}
}

func TestMessagesGet(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		convs: map[string][]history.Conversation{"demo": {{ProjectName: "demo", ContextID: "ctx-1"}}},
		msgs: map[string][]history.Message{
			"demo|ctx-1": {
				{Seq: 1, Role: "user", Content: "hi", CreatedAt: now},
				{Seq: 2, Role: "assistant", Content: "hello", CreatedAt: now},
			},
		},
	}
	rest := NewMessagesREST(reader)

	obj, err := rest.Get(nsCtx("demo"), "ctx-1", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	msgs := obj.(*assistant.ConversationMessages)
	if msgs.Name != "ctx-1" || msgs.Namespace != "demo" {
		t.Errorf("meta = %q/%q", msgs.Namespace, msgs.Name)
	}
	if len(msgs.Items) != 2 || msgs.Items[0].Role != "user" || msgs.Items[1].Content != "hello" {
		t.Fatalf("items = %+v", msgs.Items)
	}
}

// A summary turn (history.Store.Compact's digest) must reach the API
// consumer with role "summary" verbatim, not silently collapsed into
// "assistant" — see docs/conversation-summarization-design.md §2.
func TestMessagesGetRendersSummaryRoleDistinctly(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		convs: map[string][]history.Conversation{"demo": {{ProjectName: "demo", ContextID: "ctx-1"}}},
		msgs: map[string][]history.Message{
			"demo|ctx-1": {
				{Seq: 1, Role: "summary", Content: "digest of earlier turns", CreatedAt: now},
				{Seq: 2, Role: "user", Content: "what's next", CreatedAt: now},
				{Seq: 3, Role: "assistant", Content: "here's the plan", CreatedAt: now},
			},
		},
	}
	rest := NewMessagesREST(reader)

	obj, err := rest.Get(nsCtx("demo"), "ctx-1", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	msgs := obj.(*assistant.ConversationMessages)
	if len(msgs.Items) != 3 {
		t.Fatalf("items = %+v, want 3", msgs.Items)
	}
	if msgs.Items[0].Role != "summary" || msgs.Items[0].Content != "digest of earlier turns" {
		t.Fatalf("items[0] = %+v, want the summary role/content preserved", msgs.Items[0])
	}
}

func TestMessagesGetUnknownConversationIsNotFound(t *testing.T) {
	rest := NewMessagesREST(&fakeReader{convs: map[string][]history.Conversation{}})
	_, err := rest.Get(nsCtx("demo"), "missing", &metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestMessagesGetNullByteNameIsBadRequest(t *testing.T) {
	rest := NewMessagesREST(&fakeReader{convs: map[string][]history.Conversation{}})
	_, err := rest.Get(nsCtx("demo"), "foo\x00bar", &metav1.GetOptions{})
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest", err)
	}
}
