// Package conversation is the bespoke REST storage backing the conversations
// aggregated apiserver. Unlike a generic (etcd/blob) registry it wraps the
// shared relational conversation store (internal/history) directly: the A2A
// chat hot path writes the conversations/messages tables, and this storage
// projects those rows into API objects. No etcd, no double-storage — the two
// sides meet only at Postgres.
//
// Conversations are born in the chat flow, never here, so there is no create.
// What this API does own is a conversation's lifecycle once it exists: archive
// (an update of spec.archived) and delete.
package conversation

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/tenant"
	"github.com/milo-os/assistant/pkg/apis/assistant"
	"github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

var conversationsResource = assistant.Resource("conversations")

// validConversationName rejects a NUL byte in the requested name. Postgres'
// text type (unlike UTF-8 itself) disallows NUL, so an unfiltered name
// reaches the driver as a raw "invalid byte sequence" error — a 500 that both
// leaks backend/SQLSTATE detail and mischaracterizes what is actually a
// malformed request. A real context id (a generated UUID-shaped string) can
// never contain one, so rejecting it here is a pure adversarial-input guard,
// never a legitimate-lookup regression.
func validConversationName(name string) bool {
	return !strings.ContainsRune(name, 0)
}

// ConversationREST serves get/list/update/delete for Conversations from the
// shared history store. Update changes spec.archived and nothing else; delete
// is a hard delete. There is no create (the chat flow creates conversations),
// no watch, and no deletecollection — a bulk, irreversible delete behind one
// verb is not something a conversation history needs.
type ConversationREST struct {
	store history.Editor
	rest.TableConvertor
}

var (
	_ rest.Storage              = (*ConversationREST)(nil)
	_ rest.Scoper               = (*ConversationREST)(nil)
	_ rest.Lister               = (*ConversationREST)(nil)
	_ rest.Getter               = (*ConversationREST)(nil)
	_ rest.Updater              = (*ConversationREST)(nil)
	_ rest.Patcher              = (*ConversationREST)(nil)
	_ rest.GracefulDeleter      = (*ConversationREST)(nil)
	_ rest.SingularNameProvider = (*ConversationREST)(nil)
)

// NewConversationREST builds the Conversation REST over the given store.
func NewConversationREST(store history.Editor) *ConversationREST {
	return &ConversationREST{
		store:          store,
		TableConvertor: rest.NewDefaultTableConvertor(conversationsResource),
	}
}

func (r *ConversationREST) New() runtime.Object     { return &assistant.Conversation{} }
func (r *ConversationREST) NewList() runtime.Object { return &assistant.ConversationList{} }
func (r *ConversationREST) Destroy()                {}
func (r *ConversationREST) NamespaceScoped() bool   { return true }
func (r *ConversationREST) GetSingularName() string { return "conversation" }

// scope resolves the caller's project and validates the name — the two checks
// every per-object verb makes, in the same order, before touching the store.
func scope(ctx context.Context, name string) (string, error) {
	project, err := tenant.ProjectFromContext(ctx, conversationsResource)
	if err != nil {
		return "", err
	}
	if !validConversationName(name) {
		return "", apierrors.NewBadRequest("invalid conversation name")
	}
	return project, nil
}

// get reads one conversation, mapping the store's not-found to the API's 404.
func (r *ConversationREST) get(ctx context.Context, project, name string) (*assistant.Conversation, error) {
	c, err := r.store.GetConversation(ctx, project, name)
	if errors.Is(err, history.ErrConversationNotFound) {
		return nil, apierrors.NewNotFound(conversationsResource, name)
	}
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	return newConversation(c), nil
}

// Get returns one conversation by name (== A2A context id) within the caller's
// project (== request namespace). Archived conversations are gettable like any
// other: archive hides a conversation from lists, not from someone who has its
// id.
func (r *ConversationREST) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	project, err := scope(ctx, name)
	if err != nil {
		return nil, err
	}
	return r.get(ctx, project, name)
}

// List returns the caller's project's conversations, newest activity first.
// By default only conversations that are not archived are listed; the field
// selector spec.archived=true lists the archive instead (spec.archived=false
// is the default spelled out). Limit is honored; label selectors and every
// other field selector are not supported.
func (r *ConversationREST) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	project, err := tenant.ProjectFromContext(ctx, conversationsResource)
	if err != nil {
		return nil, err
	}
	opts := history.ListOptions{}
	if options != nil {
		if options.Limit > 0 {
			opts.Limit = int(options.Limit)
		}
		if opts.Archived, err = archivedSelector(options.FieldSelector); err != nil {
			return nil, err
		}
	}
	convs, err := r.store.ListConversations(ctx, project, opts)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	list := &assistant.ConversationList{Items: make([]assistant.Conversation, 0, len(convs))}
	for _, c := range convs {
		list.Items = append(list.Items, *newConversation(c))
	}
	return list, nil
}

// archivedSelector reads the one field selector List serves. Anything else is
// a 400 rather than being ignored: a client that asks for metadata.name=x and
// silently gets the whole project back has been told something false, and
// "!=" or a repeated requirement would each need semantics nobody has asked
// for.
func archivedSelector(sel fields.Selector) (bool, error) {
	if sel == nil || sel.Empty() {
		return false, nil
	}
	reqs := sel.Requirements()
	if len(reqs) != 1 || reqs[0].Field != v1alpha1.ConversationArchivedField ||
		(reqs[0].Operator != selection.Equals && reqs[0].Operator != selection.DoubleEquals) {
		return false, apierrors.NewBadRequest(
			"unsupported field selector " + sel.String() + ": conversations support only spec.archived=true or spec.archived=false")
	}
	switch reqs[0].Value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, apierrors.NewBadRequest(
		"unsupported field selector " + sel.String() + ": spec.archived must be true or false")
}

// Update archives or unarchives a conversation — PUT, and every PATCH flavor,
// since the generic handler implements patch as get + update. spec.archived is
// the only field it reads from the submitted object: status is server-owned,
// and metadata beyond name/namespace describes a row this API does not let a
// client reshape, so a client that round-trips a stale GET (kubectl replace)
// cannot write anything back but the flag. It never creates, whatever
// forceAllowCreate says: conversations are born in the chat flow, and a PUT to
// an unknown name is a 404.
//
// Toggling archive leaves last-active untouched, for the same reason rename
// does — it is not activity in the conversation, and reordering the list under
// the user's cursor would be a surprise.
func (r *ConversationREST) Update(
	ctx context.Context,
	name string,
	objInfo rest.UpdatedObjectInfo,
	_ rest.ValidateObjectFunc,
	updateValidation rest.ValidateObjectUpdateFunc,
	_ bool,
	options *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	project, err := scope(ctx, name)
	if err != nil {
		return nil, false, err
	}
	old, err := r.get(ctx, project, name)
	if err != nil {
		return nil, false, err
	}
	obj, err := objInfo.UpdatedObject(ctx, old)
	if err != nil {
		return nil, false, err
	}
	updated, ok := obj.(*assistant.Conversation)
	if !ok {
		return nil, false, apierrors.NewBadRequest("expected a Conversation")
	}
	// Two ways to say "only if this is still the conversation I read": a
	// precondition (how PUT's body arrives through UpdatedObjectInfo), or a
	// uid inside the submitted object itself (how a patch that names one
	// arrives). The uid is otherwise ignored like the rest of metadata, but a
	// mismatch is refused rather than silently applied to a different
	// incarnation of the conversation.
	if err := checkUID(name, objInfo.Preconditions(), old); err != nil {
		return nil, false, err
	}
	if updated.UID != "" {
		if err := checkUID(name, &metav1.Preconditions{UID: &updated.UID}, old); err != nil {
			return nil, false, err
		}
	}
	// An empty name/namespace is how a body that omitted them decodes; a
	// different one is an attempt to address another object through this URL.
	if updated.Name != "" && updated.Name != name {
		return nil, false, apierrors.NewBadRequest("metadata.name " + updated.Name + " does not match the conversation " + name)
	}
	if updated.Namespace != "" && updated.Namespace != project {
		return nil, false, apierrors.NewBadRequest("metadata.namespace " + updated.Namespace + " does not match the project " + project)
	}
	if updateValidation != nil {
		if err := updateValidation(ctx, updated, old); err != nil {
			return nil, false, err
		}
	}

	archived := updated.Spec.Archived
	if options != nil && len(options.DryRun) > 0 {
		return projectArchived(old, archived), false, nil
	}
	if archived != old.Spec.Archived {
		err := r.store.SetArchived(ctx, project, name, archived)
		if errors.Is(err, history.ErrConversationNotFound) {
			// Deleted between the read above and this write.
			return nil, false, apierrors.NewNotFound(conversationsResource, name)
		}
		if err != nil {
			return nil, false, apierrors.NewInternalError(err)
		}
	}
	// Read back rather than patching old in memory, so the response carries
	// the store's own archive time.
	current, err := r.get(ctx, project, name)
	if err != nil {
		return nil, false, err
	}
	return current, false, nil
}

// projectArchived is what an update would return, for a dry run that must not
// write: old with the archive flag applied, and an archive time that follows
// the store's rule (a conversation already archived keeps its time).
func projectArchived(old *assistant.Conversation, archived bool) *assistant.Conversation {
	out := old.DeepCopy()
	out.Spec.Archived = archived
	switch {
	case !archived:
		out.Status.ArchivedAt = nil
	case out.Status.ArchivedAt == nil:
		now := metav1.NewTime(time.Now())
		out.Status.ArchivedAt = &now
	}
	return out
}

// Delete permanently removes a conversation and its transcript, and returns
// the conversation as it was — deleted immediately, never "accepted", since
// there is no finalization for anything to wait on. A missing conversation is
// a 404, including one that disappeared between the read and the delete.
func (r *ConversationREST) Delete(
	ctx context.Context,
	name string,
	deleteValidation rest.ValidateObjectFunc,
	options *metav1.DeleteOptions,
) (runtime.Object, bool, error) {
	project, err := scope(ctx, name)
	if err != nil {
		return nil, false, err
	}
	existing, err := r.get(ctx, project, name)
	if err != nil {
		return nil, false, err
	}
	if options != nil {
		if err := checkUID(name, options.Preconditions, existing); err != nil {
			return nil, false, err
		}
	}
	if deleteValidation != nil {
		if err := deleteValidation(ctx, existing); err != nil {
			return nil, false, err
		}
	}
	if options != nil && len(options.DryRun) > 0 {
		return existing, true, nil
	}
	err = r.store.Delete(ctx, project, name)
	if errors.Is(err, history.ErrConversationNotFound) {
		return nil, false, apierrors.NewNotFound(conversationsResource, name)
	}
	if err != nil {
		return nil, false, apierrors.NewInternalError(err)
	}
	return existing, true, nil
}

// checkUID enforces a UID precondition — a client saying "only if this is
// still the conversation I read". It is the one precondition this storage can
// honor: there is no resourceVersion to compare, so a resourceVersion
// precondition is not checked.
func checkUID(name string, pre *metav1.Preconditions, current *assistant.Conversation) error {
	if pre == nil || pre.UID == nil || *pre.UID == current.UID {
		return nil
	}
	return apierrors.NewConflict(conversationsResource, name,
		errors.New("the UID in the precondition ("+string(*pre.UID)+") does not match the conversation's ("+string(current.UID)+")"))
}

// conversationUIDSpace namespaces the name-based UUIDs [conversationUID] mints,
// so they can never coincide with a UUID derived the same way for anything else.
var conversationUIDSpace = uuid.MustParse("6f3c1b0e-5a0d-4d7e-9a53-2f1f4a9e8c11")

// conversationUID is a conversation's metadata.uid. The store keeps no UID
// column, but the API needs one, and not only for clients that key on it: the
// generic PATCH handler reads an object without a UID as one that does not
// exist yet and takes its create-on-patch path, so without it every merge or
// JSON patch of an existing conversation answers 404. It is derived rather
// than stored — a name-based UUID over the project, the context id and the
// creation time — so it is stable across reads, differs between projects that
// share a context id, and changes if a context id is ever reused after a
// delete, which is the case a UID precondition exists to catch.
func conversationUID(c history.Conversation) types.UID {
	key := c.ProjectName + "\x00" + c.ContextID + "\x00" + c.CreatedAt.UTC().Format(time.RFC3339Nano)
	return types.UID(uuid.NewSHA1(conversationUIDSpace, []byte(key)).String())
}

// newConversation maps a stored conversation to the internal API object.
// MessageCount surfaces the stored turn (exchange) count — the store tracks
// turns, not a live message-row count; see the storage README/report.
// Spec.Archived is derived from the archive time rather than stored beside it,
// so the two can never disagree.
func newConversation(c history.Conversation) *assistant.Conversation {
	conv := &assistant.Conversation{
		ObjectMeta: metav1.ObjectMeta{
			Name:              c.ContextID,
			Namespace:         c.ProjectName,
			UID:               conversationUID(c),
			CreationTimestamp: metav1.NewTime(c.CreatedAt),
		},
		Spec: assistant.ConversationSpec{Archived: !c.ArchivedAt.IsZero()},
		Status: assistant.ConversationStatus{
			LastActiveAt: metav1.NewTime(c.LastActiveAt),
			MessageCount: int32(c.TurnCount),
			Title:        c.Title,
			Name:         c.Name,
		},
	}
	if !c.ArchivedAt.IsZero() {
		at := metav1.NewTime(c.ArchivedAt)
		conv.Status.ArchivedAt = &at
	}
	return conv
}
