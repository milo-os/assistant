package conversation

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/tenant"
	"github.com/milo-os/assistant/pkg/apis/assistant"
)

// MessagesREST serves the `conversations/messages` subresource: the whole
// transcript of one conversation embedded in a single response (v1 does not
// paginate). It is a read-only Getter keyed by the parent conversation name.
type MessagesREST struct {
	reader history.Reader
}

var (
	_ rest.Storage = (*MessagesREST)(nil)
	_ rest.Scoper  = (*MessagesREST)(nil)
	_ rest.Getter  = (*MessagesREST)(nil)
)

// NewMessagesREST builds the messages subresource REST over the read store.
func NewMessagesREST(reader history.Reader) *MessagesREST {
	return &MessagesREST{reader: reader}
}

func (r *MessagesREST) New() runtime.Object   { return &assistant.ConversationMessages{} }
func (r *MessagesREST) Destroy()              {}
func (r *MessagesREST) NamespaceScoped() bool { return true }

// Get returns the full transcript of the named conversation. A missing parent
// conversation is a 404 (rather than an empty transcript) so the subresource
// mirrors the parent's existence.
func (r *MessagesREST) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	project, err := tenant.ProjectFromContext(ctx, conversationsResource)
	if err != nil {
		return nil, err
	}
	if !validConversationName(name) {
		return nil, apierrors.NewBadRequest("invalid conversation name")
	}
	if _, err := r.reader.GetConversation(ctx, project, name); errors.Is(err, history.ErrConversationNotFound) {
		return nil, apierrors.NewNotFound(conversationsResource, name)
	} else if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	msgs, err := r.reader.Messages(ctx, project, name)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	out := &assistant.ConversationMessages{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
		Items:      make([]assistant.ConversationMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		out.Items = append(out.Items, assistant.ConversationMessage{
			Seq:       m.Seq,
			Role:      m.Role,
			Content:   m.Content,
			CreatedAt: metav1.NewTime(m.CreatedAt),
		})
	}
	return out, nil
}
