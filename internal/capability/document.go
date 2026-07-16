// Package capability owns the assistant's capability-document schema and the
// composition that turns a project's documents into the model's system-prompt
// knowledge and provider tools.
//
// This is the contract inversion made concrete: the assistant, not the
// control plane, owns the capability-document type. The on-the-wire JSON shape
// is still the services.miloapis.com/v1alpha1 AgentBinding projection (so
// existing fixtures parse unchanged), but the Go types here are
// assistant-native and carry no control-plane client. A "capability provider"
// publishes documents; how they reach the assistant is a [CapabilitySource]
// concern (fixture file today, an HTTP source later).
package capability

import (
	"encoding/json"
	"fmt"
)

// GVKRef is the {group, kind} reference style used across the service catalog
// (no version).
type GVKRef struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
}

// KnowledgeSourceType is the kind of a knowledge source document.
type KnowledgeSourceType string

const (
	KnowledgeLLMDocs  KnowledgeSourceType = "LLMDocs"
	KnowledgeRunbook  KnowledgeSourceType = "Runbook"
	KnowledgeMarkdown KnowledgeSourceType = "Markdown"
)

func (t KnowledgeSourceType) valid() bool {
	switch t {
	case KnowledgeLLMDocs, KnowledgeRunbook, KnowledgeMarkdown:
		return true
	default:
		return false
	}
}

// KnowledgeSource is a fetchable provider document.
type KnowledgeSource struct {
	Type  KnowledgeSourceType `json:"type"`
	Title string              `json:"title,omitempty"`
	URL   string              `json:"url"`
}

// KnowledgeConcept is a short, provider-authored gloss on one of its resource
// kinds.
type KnowledgeConcept struct {
	GVK     GVKRef `json:"gvk"`
	Summary string `json:"summary"`
}

// Knowledge is the Tier-1 knowledge a provider contributes.
type Knowledge struct {
	Sources  []KnowledgeSource  `json:"sources,omitempty"`
	Concepts []KnowledgeConcept `json:"concepts,omitempty"`
}

// ToolSelector is the client-side allow-list of tool names to expose from an
// MCP server.
type ToolSelector struct {
	Include []string `json:"include,omitempty"`
}

// MCPServer is one provider MCP server the assistant may connect to.
type MCPServer struct {
	Name         string       `json:"name"`
	Endpoint     string       `json:"endpoint"`
	ToolSelector ToolSelector `json:"toolSelector"`
	Mutating     []string     `json:"mutating,omitempty"`
}

// Tools is the Tier-2 tool surface a provider contributes.
type Tools struct {
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
}

// Skill is a provider-published, reviewed procedure the assistant may follow
// — the middle rung between knowledge (facts) and tools (callable endpoints).
// Only Name and Description enter the prompt; the body at Source is fetched
// on demand via the built-in load_skill tool (progressive disclosure), so a
// provider can publish many skills at near-zero prompt cost. A skill never
// grants privileges: it can only direct the model toward tools that are
// independently allow-listed.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Source is the HTTP(S) URL of the skill body (markdown/plain text).
	Source string `json:"source"`
}

// AuthorityRead names a resource kind the agent is authorized to read.
type AuthorityRead struct {
	GVK GVKRef `json:"gvk"`
}

// Authority describes the read scope and time budget granted to the agent.
type Authority struct {
	Reads                  []AuthorityRead `json:"reads,omitempty"`
	MaxTaskDurationSeconds *int            `json:"maxTaskDurationSeconds,omitempty"`
}

// Ref is a by-name object reference.
type Ref struct {
	Name string `json:"name"`
}

// CapabilitySpec is the meat of a capability document: which provider service
// it entitles and the knowledge/tools/authority it projects.
type CapabilitySpec struct {
	ServiceRef           Ref        `json:"serviceRef"`
	ServiceName          string     `json:"serviceName"`
	ServiceAgentRef      Ref        `json:"serviceAgentRef"`
	ConfigurationVersion string     `json:"configurationVersion"`
	Knowledge            *Knowledge `json:"knowledge,omitempty"`
	Tools                *Tools     `json:"tools,omitempty"`
	Skills               []Skill    `json:"skills,omitempty"`
	Authority            *Authority `json:"authority,omitempty"`
}

// Metadata mirrors the object metadata carried on the CRD projection.
type Metadata struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// Condition is a status condition on the document.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Status carries the document's status conditions.
type Status struct {
	Conditions []Condition `json:"conditions,omitempty"`
}

// CapabilityDocument is one project-scoped entitlement: a provider service and
// the capabilities it grants the assistant. The JSON shape matches the CRD
// projection; unknown fields are ignored on parse so newer projections never
// break an older assistant.
type CapabilityDocument struct {
	APIVersion string         `json:"apiVersion,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	Metadata   *Metadata      `json:"metadata,omitempty"`
	Spec       CapabilitySpec `json:"spec"`
	Status     *Status        `json:"status,omitempty"`
}

// Validate reports whether the document satisfies the required-field
// constraints (the Go analogue of the zod schema). It returns a clear,
// path-qualified error on the first violation. Unknown fields are not an
// error; missing or empty required fields are.
func (d *CapabilityDocument) Validate() error {
	s := d.Spec
	if s.ServiceName == "" {
		return fmt.Errorf("spec.serviceName: required")
	}
	if s.ServiceRef.Name == "" {
		return fmt.Errorf("spec.serviceRef.name: required")
	}
	if s.ServiceAgentRef.Name == "" {
		return fmt.Errorf("spec.serviceAgentRef.name: required")
	}
	if s.ConfigurationVersion == "" {
		return fmt.Errorf("spec.configurationVersion: required")
	}
	if s.Tools != nil {
		for i, srv := range s.Tools.MCPServers {
			if srv.Name == "" {
				return fmt.Errorf("spec.tools.mcpServers[%d].name: required", i)
			}
			if srv.Endpoint == "" {
				return fmt.Errorf("spec.tools.mcpServers[%d].endpoint: required", i)
			}
		}
	}
	for i, sk := range s.Skills {
		if sk.Name == "" {
			return fmt.Errorf("spec.skills[%d].name: required", i)
		}
		if sk.Description == "" {
			return fmt.Errorf("spec.skills[%d].description: required", i)
		}
		if sk.Source == "" {
			return fmt.Errorf("spec.skills[%d].source: required", i)
		}
	}
	if s.Knowledge != nil {
		for i, src := range s.Knowledge.Sources {
			if !src.Type.valid() {
				return fmt.Errorf("spec.knowledge.sources[%d].type: invalid %q", i, src.Type)
			}
			if src.URL == "" {
				return fmt.Errorf("spec.knowledge.sources[%d].url: required", i)
			}
		}
	}
	return nil
}

// ParseDocuments parses a capability-document fixture: either a bare JSON array
// of documents or a List object ({"items": [...]}). Entries that fail
// [CapabilityDocument.Validate] are skipped and reported to onSkip (if
// non-nil) rather than failing the whole file, so one malformed document never
// takes down a project's capabilities. A malformed root (bad JSON, or neither
// array nor list) is a hard error.
func ParseDocuments(raw []byte, onSkip func(index int, err error)) ([]CapabilityDocument, error) {
	var items []json.RawMessage

	if err := json.Unmarshal(raw, &items); err != nil {
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if listErr := json.Unmarshal(raw, &list); listErr != nil {
			return nil, fmt.Errorf("capability documents must be a JSON array or a List object with an \"items\" array: %w", err)
		}
		if list.Items == nil {
			return nil, fmt.Errorf("capability documents must be a JSON array or a List object with an \"items\" array")
		}
		items = list.Items
	}

	docs := make([]CapabilityDocument, 0, len(items))
	for i, item := range items {
		var doc CapabilityDocument
		if err := json.Unmarshal(item, &doc); err != nil {
			if onSkip != nil {
				onSkip(i, err)
			}
			continue
		}
		if err := doc.Validate(); err != nil {
			if onSkip != nil {
				onSkip(i, err)
			}
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}
