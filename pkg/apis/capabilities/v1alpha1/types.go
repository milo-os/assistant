package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ----------------------------------------------------------------------------
// CapabilityBinding — one project's entitlement to one provider service.
// ----------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=capbinding;capbindings,categories=assistant
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceName`
// +kubebuilder:printcolumn:name="Config",type=string,JSONPath=`.spec.configurationVersion`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
// +genclient:nonNamespaced

// CapabilityBinding is one project-scoped entitlement: a provider service and
// the knowledge, tools, and skills it grants that project's assistant.
//
// CLUSTER-SCOPED, and the scope is the tenancy statement. A Milo project is a
// virtual control plane — one apiserver partitioned by an etcd key prefix
// ("/projects/<project>/...") — so an object written through a project's
// control-plane path is already inside that project and can never be read
// through another's. The project plane has its own independent namespace set
// (milo-system, default); nothing creates a namespace named after the project,
// so namespacing this object would have expressed a partition that does not
// exist while making "which namespace does the assistant LIST?" an unanswerable
// question. The service catalog's AgentBinding — the producer this type mirrors
// — is cluster-scoped for exactly this reason, and its controller writes these
// with a name and no namespace.
//
// The failure mode avoided: an assistant that LISTed
// /namespaces/<project>/capabilitybindings against a project plane would get a
// well-formed, permanently empty list, and every project would silently compose
// as though it had bought nothing.
//
// A provider does not write these directly in the platform: the service catalog
// materializes one per (project, entitled service) from its own registration
// and entitlement records. Dev and e2e write them by hand, which is exactly the
// point of the assistant owning the schema.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type CapabilityBinding struct {
	metav1.TypeMeta `json:",inline"`
	// No namespace: the control plane this object lives in IS the entitled
	// project. Name is the producer's choice, conventionally the service's
	// short name (the catalog's AgentBinding controller uses the agent name).
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec CapabilityBindingSpec `json:"spec"`

	// +optional
	Status CapabilityBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CapabilityBindingList is a list of CapabilityBinding objects.
type CapabilityBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CapabilityBinding `json:"items"`
}

// CapabilityBindingSpec is the meat of a capability document: which provider
// service it entitles and the knowledge/tools/authority it projects.
//
// Every JSON tag below is a byte-for-byte match of the Go type in
// internal/capability/document.go. The capability source marshals objects of
// this type and feeds the bytes straight through capability.ParseDocuments, so
// a tag that drifts here does not fail a build — it silently drops a field from
// every project's prompt. Change both or neither.
type CapabilityBindingSpec struct {
	// ServiceRef is the catalog's stable, short identifier for the provider
	// service ("streamco"). It namespaces skills and is sanitized before it
	// reaches any field a consumer treats as an ID.
	// +required
	ServiceRef Ref `json:"serviceRef"`

	// ServiceName is the fully-qualified service name
	// ("streaming.streamco.example"). It is the dimension tool-invocation
	// metering is reported under, so an empty or wrong value misattributes
	// usage — hence required rather than derived.
	// +required
	// +kubebuilder:validation:MinLength=1
	ServiceName string `json:"serviceName"`

	// ServiceAgentRef names the provider's agent registration this binding
	// projects.
	// +required
	ServiceAgentRef Ref `json:"serviceAgentRef"`

	// ConfigurationVersion is the PROVIDER's own revision of the reviewed
	// configuration this binding carries — the review gate's audit handle,
	// distinct from metadata.generation (which only tracks this object) and
	// from the document schema version. Required so that a capability that
	// reached a customer's prompt can always be traced back to the exact
	// reviewed configuration it came from.
	// +required
	// +kubebuilder:validation:MinLength=1
	ConfigurationVersion string `json:"configurationVersion"`

	// Knowledge is the Tier-1 knowledge this service contributes.
	// +optional
	Knowledge *Knowledge `json:"knowledge,omitempty"`

	// Tools is the Tier-2 tool surface this service contributes.
	// +optional
	Tools *Tools `json:"tools,omitempty"`

	// Skills are reviewed procedures; only name and description enter the
	// prompt, the body is fetched on demand.
	// +optional
	// +listType=atomic
	Skills []Skill `json:"skills,omitempty"`

	// Authority describes the read scope and time budget granted to the agent.
	// +optional
	Authority *Authority `json:"authority,omitempty"`

	// ReportingProject is the Milo project where this service's own team
	// reviews capability-gap reports (see internal/gapreport) — resolved by
	// the service catalog from its own service registration, distinct from the
	// consumer project whose control plane holds this binding.
	// Optional: when a binding declares Tools but no ReportingProject,
	// capability-gap reporting is simply unavailable for that service (no tool
	// is registered) rather than an error.
	// +optional
	ReportingProject string `json:"reportingProject,omitempty"`
}

// Ref is a by-name object reference.
type Ref struct {
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// GVKRef is the {group, kind} reference style used across the service catalog
// (no version).
type GVKRef struct {
	// +optional
	Group string `json:"group,omitempty"`
	// +required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
}

// KnowledgeSourceType is the kind of a knowledge source document.
// +kubebuilder:validation:Enum=LLMDocs;Runbook;Markdown
type KnowledgeSourceType string

const (
	KnowledgeLLMDocs  KnowledgeSourceType = "LLMDocs"
	KnowledgeRunbook  KnowledgeSourceType = "Runbook"
	KnowledgeMarkdown KnowledgeSourceType = "Markdown"
)

// KnowledgeSource is a fetchable provider document. The assistant fetches it
// per turn under a short timeout and a byte cap; an unreachable source degrades
// the turn, it does not fail it.
type KnowledgeSource struct {
	// +required
	Type KnowledgeSourceType `json:"type"`
	// Title is what the provenance header attributes the text to; the URL is
	// used when it is empty.
	// +optional
	Title string `json:"title,omitempty"`
	// +required
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`
}

// KnowledgeConcept is a short, provider-authored gloss on one of its resource
// kinds.
type KnowledgeConcept struct {
	// +required
	GVK GVKRef `json:"gvk"`
	// +required
	// +kubebuilder:validation:MinLength=1
	Summary string `json:"summary"`
}

// Knowledge is the Tier-1 knowledge a provider contributes.
type Knowledge struct {
	// +optional
	// +listType=atomic
	Sources []KnowledgeSource `json:"sources,omitempty"`
	// +optional
	// +listType=atomic
	Concepts []KnowledgeConcept `json:"concepts,omitempty"`
}

// ToolSelector is the allow-list of tool names to expose from an MCP server.
type ToolSelector struct {
	// Include is the reviewed allow-list. It is not a hint: a tool the server
	// advertises but this list omits is never exposed to the model, and the AI
	// gateway enforces the same list independently. An empty Include therefore
	// means "no tools from this server", never "all of them".
	// +optional
	// +listType=atomic
	Include []string `json:"include,omitempty"`
}

// MCPServer is one provider MCP server the assistant may connect to.
type MCPServer struct {
	// Name namespaces this server's tools as "<name>__<tool>", so two
	// providers may publish the same tool name.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +required
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`
	// +optional
	ToolSelector ToolSelector `json:"toolSelector"`
	// Mutating names the tools in Include that change provider state, so the
	// assistant can hold them to a confirmation policy instead of treating
	// every allow-listed call as a safe read.
	// +optional
	// +listType=atomic
	Mutating []string `json:"mutating,omitempty"`
}

// Tools is the Tier-2 tool surface a provider contributes.
type Tools struct {
	// +optional
	// +listType=atomic
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
}

// Skill is a provider-published, reviewed procedure the assistant may follow
// — the middle rung between knowledge (facts) and tools (callable endpoints).
// Only Name and Description enter the prompt; the body at Source is fetched on
// demand via the built-in load_skill tool (progressive disclosure), so a
// provider can publish many skills at near-zero prompt cost. A skill never
// grants privileges: it can only direct the model toward tools that are
// independently allow-listed.
type Skill struct {
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Description is the one line that enters the prompt, and therefore the
	// only thing the model can match a request against — required because a
	// skill nobody can select is prompt cost with no payoff.
	// +required
	// +kubebuilder:validation:MinLength=1
	Description string `json:"description"`
	// Source is the HTTP(S) URL of the skill body (markdown/plain text).
	// +required
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source"`
}

// AuthorityRead names a resource kind the agent is authorized to read.
type AuthorityRead struct {
	// +required
	GVK GVKRef `json:"gvk"`
}

// Authority describes the read scope and time budget granted to the agent.
type Authority struct {
	// +optional
	// +listType=atomic
	Reads []AuthorityRead `json:"reads,omitempty"`
	// MaxTaskDurationSeconds bounds how long a task driven by this service's
	// tools may run. Nil means the assistant's own default applies.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxTaskDurationSeconds *int64 `json:"maxTaskDurationSeconds,omitempty"`
}

// ----------------------------------------------------------------------------
// Status
// ----------------------------------------------------------------------------

// CapabilityBindingStatus is the assistant's report back to whoever wrote the
// binding. It is the ONLY feedback channel a capability producer has: the
// assistant degrades rather than fails on a bad document (a skipped binding
// never surfaces in a chat as an error), so without a written status a producer
// cannot tell "the assistant is serving this" from "the assistant silently
// dropped it two weeks ago".
type CapabilityBindingStatus struct {
	// ObservedGeneration is the metadata.generation the conditions below were
	// computed from. Conditions whose observedGeneration trails
	// metadata.generation describe the PREVIOUS spec — a producer that just
	// wrote an update must wait for this to catch up before believing a stale
	// True.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the assistant's verdict on this binding. Types are
	// CapabilityBindingConditionAccepted and
	// CapabilityBindingConditionComposed; both are written by the assistant
	// only. No other controller owns a condition here.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

const (
	// CapabilityBindingConditionAccepted reports whether the assistant parsed
	// and validated this spec and will compose it for the project whose
	// control plane holds it.
	// True is a claim about ENTITLEMENT, not health: it says nothing about
	// whether the MCP endpoints answer or the knowledge URLs resolve.
	//
	// False means the assistant DROPPED the binding, and reason/message carry
	// the same path-qualified error capability.CapabilityDocument.Validate
	// returns today (e.g. "spec.skills[0].source: required"). That is not
	// redundant with CRD schema validation: admission rejects what the OpenAPI
	// schema can express, while this condition also catches what a NEWER
	// assistant refuses in a binding an OLDER CRD admitted.
	//
	// Written by the assistant, and only by the assistant.
	CapabilityBindingConditionAccepted = "Accepted"

	// CapabilityBindingConditionComposed reports whether the last turn that
	// consulted this binding could actually build it — knowledge fetched, MCP
	// servers connected, skills indexed.
	//
	// This is the condition that pays for the migration. Today a provider whose
	// MCP endpoint is unreachable learns nothing: capability.mcp.connect_failed
	// is a log line inside somebody else's service, and the customer just sees
	// an assistant that quietly cannot do the thing. As a condition it is one
	// `kubectl describe` away for the team that owns the endpoint.
	//
	// Because it describes per-turn reachability it is inherently lagging and
	// can flap; a writer must rate-limit and coalesce rather than issue an API
	// call per turn. Written by the assistant.
	CapabilityBindingConditionComposed = "Composed"
)
