package assistant

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ----------------------------------------------------------------------------
// Conversation — one durable chat conversation.
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

// Conversation is one durable chat conversation. name == the A2A context id;
// namespace == the milo project. Read-only in v1 (populated by the chat flow,
// surfaced here for list/get).
type Conversation struct {
	metav1.TypeMeta
	// Name = context_id, Namespace = project, CreationTimestamp = created_at.
	metav1.ObjectMeta

	Status ConversationStatus
}

// ConversationStatus reports rollup information about a conversation.
type ConversationStatus struct {
	// LastActiveAt is the timestamp of the most recent message.
	LastActiveAt metav1.Time
	// MessageCount is the number of stored messages.
	MessageCount int32
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type ConversationList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []Conversation
}

// ----------------------------------------------------------------------------
// ConversationMessages — the `conversations/messages` subresource object.
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ConversationMessages is the object returned by the `conversations/messages`
// subresource — the full transcript embedded.
type ConversationMessages struct {
	metav1.TypeMeta
	// Name/Namespace echo the parent conversation.
	metav1.ObjectMeta

	Items []ConversationMessage
}

// ConversationMessage is a single stored message in a conversation.
type ConversationMessage struct {
	Seq       int64
	Role      string
	Content   string
	CreatedAt metav1.Time
}
