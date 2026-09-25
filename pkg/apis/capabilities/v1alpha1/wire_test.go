package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/milo-os/assistant/internal/capability"
)

// TestSpecWireShapeMatchesCapabilityDocument is the guard on the one coupling
// that cannot fail loudly on its own: the capability source marshals a
// CapabilityBinding and hands the bytes to capability.ParseDocuments, so a
// JSON tag that drifts
// between this package and internal/capability/document.go still compiles, still
// applies, and simply drops a field out of every project's prompt.
//
// It fills EVERY field with a distinguishable value and asserts the parsed
// document is field-for-field equal. A renamed or dropped tag shows up as a zero
// value on the far side.
func TestSpecWireShapeMatchesCapabilityDocument(t *testing.T) {
	maxDur := int64(900)
	binding := CapabilityBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: SchemeGroupVersion.String(),
			Kind:       "CapabilityBinding",
		},
		// Cluster-scoped: no namespace. The project is the control plane the
		// object was read from, not a field on the object.
		ObjectMeta: metav1.ObjectMeta{Name: "streamco-binding"},
		Spec: CapabilityBindingSpec{
			ServiceRef:           Ref{Name: "streamco"},
			ServiceName:          "streaming.streamco.example",
			ServiceAgentRef:      Ref{Name: "streamco-agent"},
			ConfigurationVersion: "v7",
			Knowledge: &Knowledge{
				Sources: []KnowledgeSource{{
					Type:  KnowledgeLLMDocs,
					Title: "StreamCo docs",
					URL:   "https://provider/llms-full.txt",
				}},
				Concepts: []KnowledgeConcept{{
					GVK:     GVKRef{Group: "streamco.example", Kind: "Stream"},
					Summary: "an ordered append-only log",
				}},
			},
			Tools: &Tools{MCPServers: []MCPServer{{
				Name:         "streamco",
				Endpoint:     "http://provider/mcp",
				ToolSelector: ToolSelector{Include: []string{"streams_list", "pipeline_restart"}},
				Mutating:     []string{"pipeline_restart"},
			}}},
			Skills: []Skill{{
				Name:        "lag-triage",
				Description: "Triage pipeline consumer lag",
				Source:      "http://provider/runbooks/lag.md",
			}},
			Authority: &Authority{
				Reads:                  []AuthorityRead{{GVK: GVKRef{Group: "streamco.example", Kind: "Pipeline"}}},
				MaxTaskDurationSeconds: &maxDur,
			},
			ReportingProject: "streamco-platform",
		},
	}

	raw, err := json.Marshal([]CapabilityBinding{binding})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	docs, err := capability.ParseDocuments(raw, func(i int, err error) {
		t.Fatalf("document %d skipped: %v", i, err)
	})
	if err != nil {
		t.Fatalf("ParseDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	got := docs[0]

	// metadata.name is what survives and what matters: it is the key the status
	// writer PATCHes back on. There is no namespace to survive — the object is
	// cluster-scoped inside its project's control plane.
	if got.Metadata == nil || got.Metadata.Name != "streamco-binding" {
		t.Fatalf("metadata.name did not survive: %+v", got.Metadata)
	}
	if got.Metadata.Namespace != "" {
		t.Fatalf("cluster-scoped binding carried a namespace: %q", got.Metadata.Namespace)
	}

	wantMax := 900
	want := capability.CapabilitySpec{
		ServiceRef:           capability.Ref{Name: "streamco"},
		ServiceName:          "streaming.streamco.example",
		ServiceAgentRef:      capability.Ref{Name: "streamco-agent"},
		ConfigurationVersion: "v7",
		Knowledge: &capability.Knowledge{
			Sources: []capability.KnowledgeSource{{
				Type:  capability.KnowledgeLLMDocs,
				Title: "StreamCo docs",
				URL:   "https://provider/llms-full.txt",
			}},
			Concepts: []capability.KnowledgeConcept{{
				GVK:     capability.GVKRef{Group: "streamco.example", Kind: "Stream"},
				Summary: "an ordered append-only log",
			}},
		},
		Tools: &capability.Tools{MCPServers: []capability.MCPServer{{
			Name:         "streamco",
			Endpoint:     "http://provider/mcp",
			ToolSelector: capability.ToolSelector{Include: []string{"streams_list", "pipeline_restart"}},
			Mutating:     []string{"pipeline_restart"},
		}}},
		Skills: []capability.Skill{{
			Name:        "lag-triage",
			Description: "Triage pipeline consumer lag",
			Source:      "http://provider/runbooks/lag.md",
		}},
		Authority: &capability.Authority{
			Reads:                  []capability.AuthorityRead{{GVK: capability.GVKRef{Group: "streamco.example", Kind: "Pipeline"}}},
			MaxTaskDurationSeconds: &wantMax,
		},
		ReportingProject: "streamco-platform",
	}

	if !reflect.DeepEqual(got.Spec, want) {
		t.Errorf("spec did not round-trip through the wire shape\n got: %+v\nwant: %+v", got.Spec, want)
	}
}
