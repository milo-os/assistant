package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	// Capability is a short description of what was missing.
	// +optional
	Capability string `json:"capability,omitempty"`
	// Summary is what the user was trying to do.
	// +optional
	Summary string `json:"summary,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type CapabilityGapReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CapabilityGapReport `json:"items"`
}
