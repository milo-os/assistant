package conversation

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/tenant"
	"github.com/milo-os/assistant/pkg/apis/assistant"
)

// fakeReader is an in-test history.Editor that records the project every call
// was scoped to, so tests can assert tenancy filtering. Listing honors the
// archived filter the way the real stores do, so the REST's field selector is
// exercised end to end.
type fakeReader struct {
	convs        map[string][]history.Conversation // project -> conversations
	msgs         map[string][]history.Message      // project|id -> messages
	lastProject  string
	lastList     history.ListOptions
	forceListErr error
	// archiveCalls counts SetArchived calls, so a no-op update can be shown
	// not to write; listCalls counts ListConversations calls, so a list that
	// must answer without the store can be shown not to read.
	archiveCalls int
	listCalls    int
}

func (f *fakeReader) ListConversations(_ context.Context, project string, opts history.ListOptions) ([]history.Conversation, error) {
	f.listCalls++
	f.lastProject = project
	f.lastList = opts
	if f.forceListErr != nil {
		return nil, f.forceListErr
	}
	var out []history.Conversation
	for _, c := range f.convs[project] {
		if !c.ArchivedAt.IsZero() == opts.Archived {
			out = append(out, c)
		}
	}
	return out, nil
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

func (f *fakeReader) SetArchived(_ context.Context, project, id string, archived bool) error {
	f.lastProject = project
	f.archiveCalls++
	for i, c := range f.convs[project] {
		if c.ContextID != id {
			continue
		}
		switch {
		case !archived:
			f.convs[project][i].ArchivedAt = time.Time{}
		case c.ArchivedAt.IsZero():
			f.convs[project][i].ArchivedAt = time.Now()
		}
		return nil
	}
	return history.ErrConversationNotFound
}

func (f *fakeReader) Delete(_ context.Context, project, id string) error {
	f.lastProject = project
	for i, c := range f.convs[project] {
		if c.ContextID == id {
			f.convs[project] = append(f.convs[project][:i], f.convs[project][i+1:]...)
			return nil
		}
	}
	return history.ErrConversationNotFound
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

// projectCtx is a request in namespace ns from a milo identity whose parent
// Project is project — the shape the milo apiserver front end stamps.
func projectCtx(ns, project string) context.Context {
	return request.WithUser(nsCtx(ns), &user.DefaultInfo{
		Name: "alice",
		Extra: map[string][]string{
			tenant.ExtraParentType: {"Project"},
			tenant.ExtraParentName: {project},
		},
	})
}

// A list from a project identity in a namespace that is not its project is
// empty, not Forbidden: the namespace controller sweeping a terminating
// project control plane lists conversations in milo-system with the project's
// identity, and a 403 there wedges the namespace (milo-os/assistant#90). The
// store is never asked, so no other project's rows can leak.
func TestConversationListMismatchedNamespaceIsEmptyWithoutStoreCall(t *testing.T) {
	reader := &fakeReader{convs: map[string][]history.Conversation{
		"project-a":   {{ProjectName: "project-a", ContextID: "a"}},
		"milo-system": {{ProjectName: "milo-system", ContextID: "planted"}},
	}}
	rest := NewConversationREST(reader)

	obj, err := rest.List(projectCtx("milo-system", "project-a"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	list, ok := obj.(*assistant.ConversationList)
	if !ok {
		t.Fatalf("List returned %T, want *assistant.ConversationList", obj)
	}
	if list.Items == nil || len(list.Items) != 0 {
		t.Fatalf("Items = %#v, want an empty (non-nil) slice", list.Items)
	}
	if reader.listCalls != 0 {
		t.Fatalf("store ListConversations called %d times, want 0", reader.listCalls)
	}
}

// The mismatch path only ever answers empty — an identity whose project is
// the namespace still gets its rows, and only its rows.
func TestConversationListMatchingIdentityReturnsRows(t *testing.T) {
	reader := &fakeReader{convs: map[string][]history.Conversation{
		"project-a": {{ProjectName: "project-a", ContextID: "a1"}, {ProjectName: "project-a", ContextID: "a2"}},
		"project-b": {{ProjectName: "project-b", ContextID: "b1"}},
	}}
	rest := NewConversationREST(reader)

	obj, err := rest.List(projectCtx("project-a", "project-a"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	items := obj.(*assistant.ConversationList).Items
	if len(items) != 2 || items[0].Name != "a1" || items[1].Name != "a2" {
		t.Fatalf("items = %+v, want project-a's two", items)
	}
	if reader.listCalls != 1 || reader.lastProject != "project-a" {
		t.Fatalf("store called %d times for %q, want once for project-a", reader.listCalls, reader.lastProject)
	}
}

// Per-object verbs keep refusing a mismatch: a token for project A that names
// an object in project B's namespace is Forbidden, never answered from B.
func TestConversationGetMismatchedNamespaceIsForbidden(t *testing.T) {
	rest := NewConversationREST(&fakeReader{convs: map[string][]history.Conversation{
		"demo": {{ProjectName: "demo", ContextID: "ctx-1"}},
	}})
	_, err := rest.Get(projectCtx("demo", "other"), "ctx-1", &metav1.GetOptions{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want Forbidden", err)
	}
}

// Update serves PATCH too, so one mismatch check covers both write verbs: a
// caller pinned to another project is refused before the store is read, the
// same way Get and Delete refuse, and unlike List, which answers empty.
func TestConversationUpdateMismatchedNamespaceIsForbidden(t *testing.T) {
	r := NewConversationREST(archiveFixture())
	obj := &assistant.Conversation{Spec: assistant.ConversationSpec{Archived: true}}
	_, _, err := r.Update(projectCtx("demo", "other"), "active", rest.DefaultUpdatedObjectInfo(obj),
		rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
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

// ── archive / delete ──────────────────────────────────────────

// archiveFixture is a project with one active and one archived conversation.
func archiveFixture() *fakeReader {
	archivedAt := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	return &fakeReader{convs: map[string][]history.Conversation{
		"demo": {
			{ProjectName: "demo", ContextID: "active", Title: "still going"},
			{ProjectName: "demo", ContextID: "filed", Title: "done with this", ArchivedAt: archivedAt},
		},
	}}
}

func listNames(t *testing.T, r *ConversationREST, opts *metainternalversion.ListOptions) []string {
	t.Helper()
	obj, err := r.List(nsCtx("demo"), opts)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, c := range obj.(*assistant.ConversationList).Items {
		names = append(names, c.Name)
	}
	return names
}

// The default list is the everyday one, so archived conversations stay out of
// it unless the caller asks for the archive by field selector.
func TestConversationListFiltersArchived(t *testing.T) {
	r := NewConversationREST(archiveFixture())

	if got := listNames(t, r, &metainternalversion.ListOptions{}); len(got) != 1 || got[0] != "active" {
		t.Fatalf("default list = %v, want [active]", got)
	}
	if got := listNames(t, r, nil); len(got) != 1 || got[0] != "active" {
		t.Fatalf("nil options list = %v, want [active]", got)
	}
	for sel, want := range map[string]string{
		"spec.archived=true":   "filed",
		"spec.archived==true":  "filed",
		"spec.archived=false":  "active",
		"spec.archived==false": "active",
	} {
		got := listNames(t, r, &metainternalversion.ListOptions{FieldSelector: fields.ParseSelectorOrDie(sel)})
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s list = %v, want [%s]", sel, got, want)
		}
	}
}

func TestConversationListHonorsLimitWithSelector(t *testing.T) {
	reader := archiveFixture()
	r := NewConversationREST(reader)
	listNames(t, r, &metainternalversion.ListOptions{Limit: 7, FieldSelector: fields.ParseSelectorOrDie("spec.archived=true")})
	if reader.lastList.Limit != 7 || !reader.lastList.Archived {
		t.Fatalf("store asked for %+v, want limit 7 on the archived side", reader.lastList)
	}
}

// A selector the storage does not serve is a 400, never silently ignored — a
// client that filtered by name and got the whole project back would be misled.
func TestConversationListRejectsOtherFieldSelectors(t *testing.T) {
	r := NewConversationREST(archiveFixture())
	for _, sel := range []string{
		"metadata.name=active",
		"spec.archived!=true",
		"spec.archived=yes",
		"spec.archived=true,metadata.name=filed",
		"status.title=x",
	} {
		_, err := r.List(nsCtx("demo"), &metainternalversion.ListOptions{FieldSelector: fields.ParseSelectorOrDie(sel)})
		if !apierrors.IsBadRequest(err) {
			t.Errorf("%s: err = %v, want BadRequest", sel, err)
		}
	}
}

func TestConversationGetReportsArchiveState(t *testing.T) {
	r := NewConversationREST(archiveFixture())
	obj, err := r.Get(nsCtx("demo"), "filed", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c := obj.(*assistant.Conversation)
	if !c.Spec.Archived || c.Status.ArchivedAt == nil || !c.Status.ArchivedAt.Time.Equal(time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("archived get = spec %+v status.archivedAt %v", c.Spec, c.Status.ArchivedAt)
	}
	obj, err = r.Get(nsCtx("demo"), "active", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c := obj.(*assistant.Conversation); c.Spec.Archived || c.Status.ArchivedAt != nil {
		t.Fatalf("active get = spec %+v status.archivedAt %v, want neither set", c.Spec, c.Status.ArchivedAt)
	}
}

// update drives Update the way the generic PUT handler does: the submitted
// object is fixed up front, independent of the current one.
func update(r *ConversationREST, name string, obj *assistant.Conversation, opts *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return r.Update(nsCtx("demo"), name, rest.DefaultUpdatedObjectInfo(obj),
		rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, opts)
}

// patch drives Update the way the generic PATCH handler does: the new object
// is computed from the current one, here by setting spec.archived.
func patch(r *ConversationREST, name string, archived bool) (runtime.Object, bool, error) {
	info := rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, newObj, oldObj runtime.Object) (runtime.Object, error) {
		out := oldObj.(*assistant.Conversation).DeepCopy()
		out.Spec.Archived = archived
		return out, nil
	})
	return r.Update(nsCtx("demo"), name, info, rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
}

func TestConversationPatchArchivesAndUnarchives(t *testing.T) {
	reader := archiveFixture()
	r := NewConversationREST(reader)

	obj, created, err := patch(r, "active", true)
	if err != nil || created {
		t.Fatalf("archive patch: created=%v err=%v", created, err)
	}
	c := obj.(*assistant.Conversation)
	if !c.Spec.Archived || c.Status.ArchivedAt == nil {
		t.Fatalf("archive returned spec %+v archivedAt %v, want both set", c.Spec, c.Status.ArchivedAt)
	}
	if got := listNames(t, r, &metainternalversion.ListOptions{}); len(got) != 0 {
		t.Fatalf("default list after archive = %v, want empty", got)
	}

	obj, _, err = patch(r, "active", false)
	if err != nil {
		t.Fatalf("unarchive patch: %v", err)
	}
	if c := obj.(*assistant.Conversation); c.Spec.Archived || c.Status.ArchivedAt != nil {
		t.Fatalf("unarchive returned spec %+v archivedAt %v, want neither", c.Spec, c.Status.ArchivedAt)
	}
}

// Archiving an already-archived conversation is a no-op that keeps the time it
// was first archived — and does not even reach the store.
func TestConversationArchiveIsIdempotent(t *testing.T) {
	reader := archiveFixture()
	r := NewConversationREST(reader)
	obj, _, err := patch(r, "filed", true)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if at := obj.(*assistant.Conversation).Status.ArchivedAt; at == nil || !at.Time.Equal(time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("archivedAt = %v, want the original time", at)
	}
	if reader.archiveCalls != 0 {
		t.Fatalf("store written %d times for a no-op update", reader.archiveCalls)
	}
}

// PUT carries a whole object, typically a stale GET round-tripped through an
// editor (kubectl replace). Only spec.archived may land; status and the rest
// of metadata are the server's.
func TestConversationPutIgnoresEverythingButSpecArchived(t *testing.T) {
	reader := archiveFixture()
	r := NewConversationREST(reader)
	put := &assistant.Conversation{
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "demo", Labels: map[string]string{"x": "y"}},
		Spec:       assistant.ConversationSpec{Archived: true},
		Status:     assistant.ConversationStatus{Title: "forged", Name: "forged", MessageCount: 999},
	}
	obj, _, err := update(r, "active", put, &metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	c := obj.(*assistant.Conversation)
	if !c.Spec.Archived {
		t.Fatal("spec.archived was not applied")
	}
	if c.Status.Title != "still going" || c.Status.Name != "" || c.Status.MessageCount != 0 || len(c.Labels) != 0 {
		t.Fatalf("submitted non-spec fields leaked into the result: %+v", c)
	}

	// A body that omits metadata entirely is still addressed by the URL.
	if _, _, err := update(r, "active", &assistant.Conversation{}, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update without metadata: %v", err)
	}
	if got, _ := reader.GetConversation(context.Background(), "demo", "active"); !got.ArchivedAt.IsZero() {
		t.Fatal("a PUT without spec.archived should unarchive — spec is replaced, not merged")
	}
}

func TestConversationUpdateRejectsMismatchedIdentity(t *testing.T) {
	r := NewConversationREST(archiveFixture())
	for name, obj := range map[string]*assistant.Conversation{
		"name":      {ObjectMeta: metav1.ObjectMeta{Name: "filed"}},
		"namespace": {ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "other"}},
	} {
		if _, _, err := update(r, "active", obj, &metav1.UpdateOptions{}); !apierrors.IsBadRequest(err) {
			t.Errorf("mismatched %s: err = %v, want BadRequest", name, err)
		}
	}
}

// Conversations are created by the chat flow; PUT never conjures one, even
// when the handler would allow create-on-update.
func TestConversationUpdateMissingIsNotFound(t *testing.T) {
	r := NewConversationREST(archiveFixture())
	obj := &assistant.Conversation{Spec: assistant.ConversationSpec{Archived: true}}
	_, _, err := r.Update(nsCtx("demo"), "missing", rest.DefaultUpdatedObjectInfo(obj),
		rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, true, &metav1.UpdateOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
	if _, _, err := patch(r, "foo\x00bar", true); !apierrors.IsBadRequest(err) {
		t.Fatalf("NUL name err = %v, want BadRequest", err)
	}
}

func TestConversationUpdateDryRunDoesNotWrite(t *testing.T) {
	reader := archiveFixture()
	r := NewConversationREST(reader)
	obj, _, err := update(r, "active", &assistant.Conversation{Spec: assistant.ConversationSpec{Archived: true}},
		&metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if c := obj.(*assistant.Conversation); !c.Spec.Archived || c.Status.ArchivedAt == nil {
		t.Fatalf("dry run should report the would-be result, got %+v", c)
	}
	if reader.archiveCalls != 0 {
		t.Fatal("dry run wrote to the store")
	}
}

func TestConversationDeleteReturnsTheDeletedObject(t *testing.T) {
	reader := archiveFixture()
	r := NewConversationREST(reader)
	obj, deleted, err := r.Delete(nsCtx("demo"), "filed", rest.ValidateAllObjectFunc, &metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !deleted {
		t.Fatal("deleted = false, want an immediate delete")
	}
	if c := obj.(*assistant.Conversation); c.Name != "filed" || c.Namespace != "demo" || c.Status.Title != "done with this" {
		t.Fatalf("returned %+v, want the conversation as it was", c)
	}
	if _, err := r.Get(nsCtx("demo"), "filed", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("get after delete err = %v, want NotFound", err)
	}
	if _, _, err := r.Delete(nsCtx("demo"), "filed", rest.ValidateAllObjectFunc, &metav1.DeleteOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("second delete err = %v, want NotFound", err)
	}
}

func TestConversationDeleteDryRunKeepsIt(t *testing.T) {
	r := NewConversationREST(archiveFixture())
	if _, deleted, err := r.Delete(nsCtx("demo"), "active", rest.ValidateAllObjectFunc,
		&metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}); err != nil || !deleted {
		t.Fatalf("dry-run delete: deleted=%v err=%v", deleted, err)
	}
	if _, err := r.Get(nsCtx("demo"), "active", &metav1.GetOptions{}); err != nil {
		t.Fatalf("dry run deleted the conversation: %v", err)
	}
}

func TestConversationDeleteGuards(t *testing.T) {
	r := NewConversationREST(archiveFixture())
	if _, _, err := r.Delete(nsCtx("demo"), "foo\x00bar", nil, nil); !apierrors.IsBadRequest(err) {
		t.Errorf("NUL name err = %v, want BadRequest", err)
	}
	if _, _, err := r.Delete(context.Background(), "active", nil, nil); !apierrors.IsBadRequest(err) {
		t.Errorf("missing namespace err = %v, want BadRequest", err)
	}
	// A project identity may not delete in another project's namespace, and
	// the conversation named in the URL is looked up only in the caller's.
	ctx := request.WithUser(nsCtx("demo"), &user.DefaultInfo{
		Name:  "alice",
		Extra: map[string][]string{tenant.ExtraParentType: {"Project"}, tenant.ExtraParentName: {"other"}},
	})
	if _, _, err := r.Delete(ctx, "active", nil, nil); !apierrors.IsForbidden(err) {
		t.Errorf("cross-project delete err = %v, want Forbidden", err)
	}
	if _, _, err := r.Delete(nsCtx("other"), "active", nil, nil); !apierrors.IsNotFound(err) {
		t.Errorf("delete in a project without it err = %v, want NotFound", err)
	}
}

// The generic PATCH handler treats an object with no UID as not existing yet,
// so every conversation must carry one — stable across reads, and distinct
// per project and per incarnation of a context id.
func TestConversationUIDIsStableAndDistinct(t *testing.T) {
	created := time.Date(2026, 7, 17, 10, 0, 0, 123456000, time.UTC)
	base := history.Conversation{ProjectName: "demo", ContextID: "ctx-1", CreatedAt: created}
	uid := newConversation(base).UID
	if uid == "" {
		t.Fatal("conversation has no UID")
	}
	if again := newConversation(base).UID; again != uid {
		t.Fatalf("UID changed across reads: %s vs %s", uid, again)
	}
	otherProject := base
	otherProject.ProjectName = "other"
	recreated := base
	recreated.CreatedAt = created.Add(time.Hour)
	if newConversation(otherProject).UID == uid || newConversation(recreated).UID == uid {
		t.Fatal("UID must differ across projects and across a recreated context id")
	}
}

func TestConversationUIDPreconditions(t *testing.T) {
	reader := archiveFixture()
	r := NewConversationREST(reader)
	obj, err := r.Get(nsCtx("demo"), "active", &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	uid := obj.(*assistant.Conversation).UID
	stale := types.UID("00000000-0000-0000-0000-000000000000")

	put := func(u types.UID) error {
		_, _, err := update(r, "active", &assistant.Conversation{
			ObjectMeta: metav1.ObjectMeta{Name: "active", UID: u},
			Spec:       assistant.ConversationSpec{Archived: true},
		}, &metav1.UpdateOptions{})
		return err
	}
	if err := put(stale); !apierrors.IsConflict(err) {
		t.Fatalf("stale UID update err = %v, want Conflict", err)
	}
	if reader.archiveCalls != 0 {
		t.Fatal("a conflicting update wrote to the store")
	}
	if err := put(uid); err != nil {
		t.Fatalf("matching UID update: %v", err)
	}

	if _, _, err := r.Delete(nsCtx("demo"), "active", nil,
		&metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &stale}}); !apierrors.IsConflict(err) {
		t.Fatalf("stale UID delete err = %v, want Conflict", err)
	}
	if _, _, err := r.Delete(nsCtx("demo"), "active", nil,
		&metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
		t.Fatalf("matching UID delete: %v", err)
	}
}
