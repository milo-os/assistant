package openapi_test

import (
	"testing"

	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
	generatedopenapi "github.com/milo-os/assistant/pkg/generated/openapi"
)

// modelNamer is what openapi-gen adds to each generated type. Every type the
// apiserver serves must implement it AND appear in the generated definitions.
type modelNamer interface{ OpenAPIModelName() string }

// The generic apiserver resolves an OpenAPI model for every type it serves
// while it builds the handler chain, and a type with no definition is fatal:
//
//	unable to get openapi models: cannot find model definition for
//	com.miloapis.assistant.pkg.apis.assistant.v1alpha1.AssistantEndpoint
//
// That is a crash loop at startup, not a degraded endpoint, and it is invisible
// to every other test — the REST storage has its own unit tests and they all
// pass, because they never build a server. It is also invisible to `go build`:
// the definitions are a map, so a missing entry is a runtime lookup, not a
// compile error.
//
// The gap appears when a type is added to pkg/apis and openapi-gen is not
// re-run, which is exactly what happened when AssistantEndpoint was introduced.
// This test is the cheap check that would have caught it.
func TestGeneratedDefinitionsCoverEveryServedType(t *testing.T) {
	// Every root type the apiserver registers storage for, plus the nested
	// types those reference.
	served := []any{
		v1alpha1.Conversation{},
		v1alpha1.ConversationList{},
		v1alpha1.ConversationStatus{},
		v1alpha1.ConversationMessage{},
		v1alpha1.ConversationMessages{},
		v1alpha1.CapabilityGapReport{},
		v1alpha1.CapabilityGapReportList{},
		v1alpha1.CapabilityGapReportStatus{},
		v1alpha1.AssistantEndpoint{},
		v1alpha1.AssistantEndpointList{},
		v1alpha1.AssistantEndpointSpec{},
	}

	defs := generatedopenapi.GetOpenAPIDefinitions(func(path string) spec.Ref { return spec.Ref{} })

	for _, obj := range served {
		namer, ok := obj.(modelNamer)
		if !ok {
			t.Errorf("%T has no OpenAPIModelName — openapi-gen has not been run for it", obj)
			continue
		}
		name := namer.OpenAPIModelName()
		if _, found := defs[name]; !found {
			t.Errorf("no OpenAPI definition for %T (%s)\n"+
				"the apiserver will fail to start with \"cannot find model definition\";\n"+
				"re-run openapi-gen over pkg/apis/assistant/v1alpha1", obj, name)
		}
	}
}

// A definition that resolves to an empty schema would satisfy the lookup above
// while still serving nothing useful, so check the new type carries real
// properties.
func TestAssistantEndpointDefinitionHasSchema(t *testing.T) {
	defs := generatedopenapi.GetOpenAPIDefinitions(func(path string) spec.Ref { return spec.Ref{} })

	def, ok := defs[v1alpha1.AssistantEndpoint{}.OpenAPIModelName()]
	if !ok {
		t.Fatal("AssistantEndpoint has no OpenAPI definition")
	}
	if len(def.Schema.Properties) == 0 {
		t.Fatal("AssistantEndpoint's definition carries no properties")
	}
	if _, ok := def.Schema.Properties["spec"]; !ok {
		t.Errorf("AssistantEndpoint's definition has no spec property, got %v",
			propertyNames(def))
	}
}

func propertyNames(def common.OpenAPIDefinition) []string {
	names := make([]string, 0, len(def.Schema.Properties))
	for k := range def.Schema.Properties {
		names = append(names, k)
	}
	return names
}
