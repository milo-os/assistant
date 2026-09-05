// Package conversation is the bespoke read-only REST storage backing the
// conversations aggregated apiserver. Unlike a generic (etcd/blob) registry it
// wraps the shared relational conversation store (internal/history) directly:
// the A2A chat hot path writes the conversations/messages tables, and this
// storage projects those rows into API objects for list/get. No etcd, no
// double-storage — the two sides meet only at Postgres.
package conversation

import (
	"context"
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/tenant"
	"github.com/milo-os/assistant/pkg/apis/assistant"
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

// ConversationREST serves list/get for Conversations from the shared history
// store. It is read-only: create/update/delete are not implemented (v1).
type ConversationREST struct {
	reader history.Reader
	rest.TableConvertor
}

var (
	_ rest.Storage              = (*ConversationREST)(nil)
	_ rest.Scoper               = (*ConversationREST)(nil)
	_ rest.Lister               = (*ConversationREST)(nil)
	_ rest.Getter               = (*ConversationREST)(nil)
	_ rest.SingularNameProvider = (*ConversationREST)(nil)
)

// NewConversationREST builds the Conversation REST over the given read store.
func NewConversationREST(reader history.Reader) *ConversationREST {
	return &ConversationREST{
		reader:         reader,
		TableConvertor: rest.NewDefaultTableConvertor(conversationsResource),
	}
}

func (r *ConversationREST) New() runtime.Object     { return &assistant.Conversation{} }
func (r *ConversationREST) NewList() runtime.Object { return &assistant.ConversationList{} }
func (r *ConversationREST) Destroy()                {}
func (r *ConversationREST) NamespaceScoped() bool   { return true }
func (r *ConversationREST) GetSingularName() string { return "conversation" }

// Get returns one conversation by name (== A2A context id) within the caller's
// project (== request namespace).
func (r *ConversationREST) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	project, err := tenant.ProjectFromContext(ctx, conversationsResource)
	if err != nil {
		return nil, err
	}
	if !validConversationName(name) {
		return nil, apierrors.NewBadRequest("invalid conversation name")
	}
	c, err := r.reader.GetConversation(ctx, project, name)
	if errors.Is(err, history.ErrConversationNotFound) {
		return nil, apierrors.NewNotFound(conversationsResource, name)
	}
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	return newConversation(c), nil
}

// List returns the caller's project's conversations, newest activity first.
// Label/field selectors are not supported in v1 (Conversations carry neither),
// so options are honored only for Limit.
func (r *ConversationREST) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	project, err := tenant.ProjectFromContext(ctx, conversationsResource)
	if err != nil {
		return nil, err
	}
	limit := 0
	if options != nil && options.Limit > 0 {
		limit = int(options.Limit)
	}
	convs, err := r.reader.ListConversations(ctx, project, limit)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	list := &assistant.ConversationList{Items: make([]assistant.Conversation, 0, len(convs))}
	for _, c := range convs {
		list.Items = append(list.Items, *newConversation(c))
	}
	return list, nil
}

// newConversation maps a stored conversation to the internal API object.
// MessageCount surfaces the stored turn (exchange) count — the store tracks
// turns, not a live message-row count; see the storage README/report.
func newConversation(c history.Conversation) *assistant.Conversation {
	return &assistant.Conversation{
		ObjectMeta: metav1.ObjectMeta{
			Name:              c.ContextID,
			Namespace:         c.ProjectName,
			CreationTimestamp: metav1.NewTime(c.CreatedAt),
		},
		Status: assistant.ConversationStatus{
			LastActiveAt: metav1.NewTime(c.LastActiveAt),
			MessageCount: int32(c.TurnCount),
			Title:        c.Title,
		},
	}
}
