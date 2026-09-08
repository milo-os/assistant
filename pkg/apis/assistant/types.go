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
	// Title is the conversation's opening user message, collapsed to one
	// line and truncated — what the conversation is about, for listings.
	Title string
	// Name is what the user called this conversation, empty until they name
	// one. Clients show it in place of Title where set.
	Name string
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

// ----------------------------------------------------------------------------
// CapabilityGapReport — a provider service's own record that it was missing a
// tool/lookup/knowledge a user needed. See internal/gapreport.
// ----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient

// CapabilityGapReport is one capability-gap report. name == the report id;
// namespace == the PROVIDER project (spec.reportingProject on the capability
// document that raised it) — never the consumer project the conversation ran
// in, which is carried only as provenance in Status. Read-only (written by
// the report_capability_gap tool, surfaced here for list/get).
type CapabilityGapReport struct {
	metav1.TypeMeta
	// Name = report id, Namespace = provider project, CreationTimestamp = created_at.
	metav1.ObjectMeta

	Status CapabilityGapReportStatus
}

// CapabilityGapReportStatus carries the report's content.
type CapabilityGapReportStatus struct {
	// ServiceName identifies the provider service the gap belongs to.
	ServiceName string
	// ConsumerProject is the project the conversation happened in — provenance only.
	ConsumerProject string
	// ContextID is the conversation the gap arose in — provenance only.
	ContextID string
	// CapabilityKey groups this occurrence with other reports of the same
	// gap. Empty on reports filed before keys existed, or without one.
	CapabilityKey string
	// Capability is a short description of the capability at fault.
	Capability string
	// Summary is what the user was trying to do.
	Summary string
	// Kind classifies the shortfall (MissingCapability, InsufficientDetail,
	// MisleadingOutput, UnactionableGuidance). Reports stored before kinds
	// existed read back as MissingCapability.
	Kind string
	// Evidence quotes the tool output a non-MissingCapability report is
	// about. Nil when there is nothing to quote.
	Evidence *CapabilityGapReportEvidence
}

// CapabilityGapReportEvidence quotes the offending tool output. It carries
// tool output and object state only — never text from the user's message.
type CapabilityGapReportEvidence struct {
	// Tool is the tool whose output was at fault.
	Tool string
	// Observed is what that tool returned.
	Observed string
	// ContradictedBy is the fact that makes Observed wrong, thin, or
	// impossible to act on.
	ContradictedBy string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type CapabilityGapReportList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []CapabilityGapReport
}

// CapabilityGap is the aggregate form of the above: see the v1alpha1 type.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient
type CapabilityGap struct {
	metav1.TypeMeta
	// Name = capability key (or the report id for a keyless report),
	// Namespace = provider project, CreationTimestamp = first seen.
	metav1.ObjectMeta

	Status CapabilityGapStatus
}

// CapabilityGapStatus carries one distinct gap and how widely it was hit.
type CapabilityGapStatus struct {
	// ServiceName identifies the provider service the gap belongs to.
	ServiceName string
	// CapabilityKey is the key the occurrences share. Empty for a gap filed
	// before keys existed, which stands alone.
	CapabilityKey string
	// Capability is the most recent occurrence's description.
	Capability string
	// Kind is the most recent occurrence's classification.
	Kind string
	// Conversations counts the distinct conversations that hit this gap.
	Conversations int32
	// Occurrences counts the reports filed; it can exceed Conversations.
	Occurrences int32
	// FirstSeen and LastSeen bound the occurrences.
	FirstSeen metav1.Time
	LastSeen  metav1.Time
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type CapabilityGapList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []CapabilityGap
}

// AssistantEndpoint is the internal form of the endpoint discovery resource.
// See the v1alpha1 type for what it is and why it exists.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AssistantEndpoint struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec AssistantEndpointSpec
}

// AssistantEndpointSpec describes how to reach the service.
type AssistantEndpointSpec struct {
	URL           string
	AgentCardPath string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AssistantEndpointList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []AssistantEndpoint
}

const (
	// AssistantEndpointName is the name of the singleton endpoint object. One
	// assistant serves the control plane, so there is exactly one.
	AssistantEndpointName = "assistant"

	// DefaultAgentCardPath is the well-known A2A agent card location.
	DefaultAgentCardPath = "/.well-known/agent-card.json"
)
