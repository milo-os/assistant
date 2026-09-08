package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/milo-os/assistant/pkg/apis/assistant"
)

// ----------------------------------------------------------------------------
// Conversation — one durable chat conversation.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=conversation
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Messages",type=integer,JSONPath=`.status.messageCount`
// +kubebuilder:printcolumn:name="LastActive",type=date,JSONPath=`.status.lastActiveAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient

// Conversation is one durable chat conversation. name == the A2A context id;
// namespace == the milo project. Read-only in v1 (populated by the chat flow,
// surfaced here for list/get).
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Conversation struct {
	metav1.TypeMeta `json:",inline"`
	// Name = context_id, Namespace = project, CreationTimestamp = created_at.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Status ConversationStatus `json:"status,omitempty"`
}

// ConversationStatus reports rollup information about a conversation.
type ConversationStatus struct {
	// LastActiveAt is the timestamp of the most recent message.
	// +optional
	LastActiveAt metav1.Time `json:"lastActiveAt,omitempty"`
	// MessageCount is the number of stored messages.
	// +optional
	MessageCount int32 `json:"messageCount,omitempty"`
	// Title is the conversation's opening user message, collapsed to one
	// line and truncated — what the conversation is about, for listings.
	// +optional
	Title string `json:"title,omitempty"`
	// Name is what the user called this conversation, empty until they name
	// one. Clients show it in place of Title where set.
	// +optional
	Name string `json:"name,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ConversationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Conversation `json:"items"`
}

// ----------------------------------------------------------------------------
// ConversationMessages — the `conversations/messages` subresource object.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ConversationMessages is the object returned by the `conversations/messages`
// subresource — the full transcript embedded.
type ConversationMessages struct {
	metav1.TypeMeta `json:",inline"`
	// Name/Namespace echo the parent conversation.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Items []ConversationMessage `json:"items"`
}

// ConversationMessage is a single stored message in a conversation.
type ConversationMessage struct {
	Seq int64 `json:"seq"`
	// +kubebuilder:validation:Enum=user;assistant
	Role      string      `json:"role"`
	Content   string      `json:"content"`
	CreatedAt metav1.Time `json:"createdAt"`
}

// ----------------------------------------------------------------------------
// CapabilityGapReport — a provider service's own record that it was missing a
// tool/lookup/knowledge a user needed. See internal/gapreport.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=gapreport
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.status.serviceName`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.status.kind`
// +kubebuilder:printcolumn:name="Capability",type=string,JSONPath=`.status.capability`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient

// CapabilityGapReport is one capability-gap report. name == the report id;
// namespace == the PROVIDER project (spec.reportingProject on the capability
// document that raised it) — never the consumer project the conversation ran
// in, which is carried only as provenance in Status. Read-only (written by
// the report_capability_gap tool, surfaced here for list/get).
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type CapabilityGapReport struct {
	metav1.TypeMeta `json:",inline"`
	// Name = report id, Namespace = provider project, CreationTimestamp = created_at.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Status CapabilityGapReportStatus `json:"status,omitempty"`
}

// CapabilityGapReportStatus carries the report's content.
type CapabilityGapReportStatus struct {
	// ServiceName identifies the provider service the gap belongs to.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`
	// ConsumerProject is the project the conversation happened in — provenance only.
	// +optional
	ConsumerProject string `json:"consumerProject,omitempty"`
	// ContextID is the conversation the gap arose in — provenance only.
	// +optional
	ContextID string `json:"contextID,omitempty"`
	// CapabilityKey groups this occurrence with every other report of the
	// same gap; it is the name of the CapabilityGap it rolls up into. Empty
	// on reports filed before keys existed, or filed without one — those
	// stand alone in the aggregate rather than being merged on a guess.
	// +optional
	CapabilityKey string `json:"capabilityKey,omitempty"`
	// Capability is a short description of the capability at fault.
	// +optional
	Capability string `json:"capability,omitempty"`
	// Summary is what the user was trying to do.
	// +optional
	Summary string `json:"summary,omitempty"`
	// Kind classifies the shortfall. Reports stored before kinds existed read
	// back as MissingCapability.
	// +optional
	Kind CapabilityGapKind `json:"kind,omitempty"`
	// Evidence quotes the tool output a non-MissingCapability report is
	// about. Absent when there is nothing to quote.
	// +optional
	Evidence *CapabilityGapReportEvidence `json:"evidence,omitempty"`
}

// CapabilityGapKind classifies what kind of shortfall a report describes: a
// gap is not only an absent tool, but also a tool that answers with too
// little, answers misleadingly, or gives guidance the user cannot act on.
// +kubebuilder:validation:Enum=MissingCapability;InsufficientDetail;MisleadingOutput;UnactionableGuidance
type CapabilityGapKind string

const (
	// CapabilityGapKindMissingCapability: no tool covered what the user needed.
	CapabilityGapKindMissingCapability CapabilityGapKind = "MissingCapability"
	// CapabilityGapKindInsufficientDetail: a tool answered, but omitted a
	// field the answer needed to be actionable.
	CapabilityGapKindInsufficientDetail CapabilityGapKind = "InsufficientDetail"
	// CapabilityGapKindMisleadingOutput: a tool answered, and its output
	// pointed at a wrong conclusion.
	CapabilityGapKindMisleadingOutput CapabilityGapKind = "MisleadingOutput"
	// CapabilityGapKindUnactionableGuidance: a tool told the user to do
	// something they cannot do.
	CapabilityGapKindUnactionableGuidance CapabilityGapKind = "UnactionableGuidance"
)

// CapabilityGapReportEvidence quotes the offending tool output so the
// provider's team can check the claim. It carries tool output and object
// state only — never text from the user's message.
type CapabilityGapReportEvidence struct {
	// Tool is the tool whose output was at fault, e.g. "workloads_list".
	// +optional
	Tool string `json:"tool,omitempty"`
	// Observed is what that tool returned, e.g. "actionability: transient".
	// +optional
	Observed string `json:"observed,omitempty"`
	// ContradictedBy is the fact that makes Observed wrong, thin, or
	// impossible to act on, e.g. "instance unchanged for 9d".
	// +optional
	ContradictedBy string `json:"contradictedBy,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type CapabilityGapReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CapabilityGapReport `json:"items"`
}

// ----------------------------------------------------------------------------
// CapabilityGap — one distinct gap, with how many conversations hit it.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=gap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.status.serviceName`
// +kubebuilder:printcolumn:name="Conversations",type=integer,JSONPath=`.status.conversations`
// +kubebuilder:printcolumn:name="Occurrences",type=integer,JSONPath=`.status.occurrences`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.status.kind`
// +kubebuilder:printcolumn:name="Capability",type=string,JSONPath=`.status.capability`
// +kubebuilder:printcolumn:name="Last-Seen",type=date,JSONPath=`.status.lastSeen`
// +genclient

// CapabilityGap is one distinct capability gap for a provider service: every
// CapabilityGapReport sharing a capability key, collapsed into a single entry
// with a count of how many conversations hit it. It is the view to prioritise
// from — the same gap described three different ways by three conversations
// is one gap here and three reports there.
//
// The individual reports stay available as capabilitygapreports and are where
// the per-occurrence evidence lives; that evidence is what makes a quality
// defect diagnosable, so the aggregate summarises it rather than replacing it.
//
// name == the capability key, or, for a report filed before keys existed, that
// report's own id — keyless reports are never merged with each other, because
// their free-prose descriptions are exactly what cannot establish that two of
// them are the same gap. namespace == the PROVIDER project, same as
// CapabilityGapReport. Read-only.
//
// It carries no consumer identity: how many conversations hit a gap is the
// prioritisation signal, and which customers they were is a separate question
// this view deliberately does not answer.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type CapabilityGap struct {
	metav1.TypeMeta `json:",inline"`
	// Name = capability key (or report id), Namespace = provider project,
	// CreationTimestamp = first seen.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Status CapabilityGapStatus `json:"status,omitempty"`
}

// CapabilityGapStatus carries one distinct gap and how widely it was hit.
type CapabilityGapStatus struct {
	// ServiceName identifies the provider service the gap belongs to. Keys
	// are per-service vocabulary: the same key on two services is two gaps.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`
	// CapabilityKey is the key every occurrence shares, e.g.
	// "workload-metrics". Empty for a gap filed before keys existed.
	// +optional
	CapabilityKey string `json:"capabilityKey,omitempty"`
	// Capability is the most recent occurrence's description — the freshest
	// wording of a gap that has been re-filed several times.
	// +optional
	Capability string `json:"capability,omitempty"`
	// Kind is the most recent occurrence's classification.
	// +optional
	Kind CapabilityGapKind `json:"kind,omitempty"`
	// Conversations is how many distinct conversations hit this gap. It
	// counts conversations, not reports, so one conversation filing twice
	// still counts once.
	// +optional
	Conversations int32 `json:"conversations,omitempty"`
	// Occurrences is how many reports were filed. It can exceed
	// Conversations.
	// +optional
	Occurrences int32 `json:"occurrences,omitempty"`
	// FirstSeen is when this gap was first reported.
	// +optional
	FirstSeen metav1.Time `json:"firstSeen,omitempty"`
	// LastSeen is when it was most recently reported.
	// +optional
	LastSeen metav1.Time `json:"lastSeen,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type CapabilityGapList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CapabilityGap `json:"items"`
}

// ----------------------------------------------------------------------------
// AssistantEndpoint — where to reach the assistant's A2A service.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=endpoint
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +genclient
// +genclient:nonNamespaced

// AssistantEndpoint advertises where clients should send A2A traffic.
//
// It exists so a client that already reaches this aggregated API — with the
// caller's own Kubernetes identity and no extra credential — can find the
// service without being told a hostname out of band. Before it, `datumctl
// patch` required PATCH_URL: the control-plane address names Milo, not the
// assistant, and nothing else advertised the assistant's address.
//
// Read-only and not stored. The service reports the address it was configured
// to advertise (PUBLIC_BASE_URL) — the same value it puts in its agent card, so
// the card and this resource cannot disagree.
//
// Cluster-scoped: one assistant serves every project on a control plane, so the
// endpoint is not a per-project fact. Named [AssistantEndpointName].
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AssistantEndpoint struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec AssistantEndpointSpec `json:"spec,omitempty"`
}

// AssistantEndpointSpec describes how to reach the service.
type AssistantEndpointSpec struct {
	// URL is the assistant's public base URL, e.g.
	// "https://patch.staging.env.datum.net". Clients append the A2A path, or
	// fetch the agent card and use the endpoint the card advertises.
	//
	// Empty when the service has no PUBLIC_BASE_URL configured: an operator has
	// not told it its own address. Empty is reported rather than guessed — a
	// wrong URL here would silently point clients at another service.
	// +optional
	URL string `json:"url,omitempty"`

	// AgentCardPath is where the A2A agent card is served, relative to URL.
	// +optional
	AgentCardPath string `json:"agentCardPath,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type AssistantEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AssistantEndpoint `json:"items"`
}

const (
	// AssistantEndpointName is the name of the singleton endpoint object. One
	// assistant serves the control plane, so there is exactly one.
	AssistantEndpointName = assistant.AssistantEndpointName

	// DefaultAgentCardPath is the well-known A2A agent card location.
	DefaultAgentCardPath = assistant.DefaultAgentCardPath
)
