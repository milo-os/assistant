package agent

import (
	"context"
	"testing"

	"github.com/milo-os/assistant/internal/capability"
)

// TestEntitlementsUsesScopeGate pins the ordering the card path depends on: a
// Source that returns another project's document (FixtureSource ignores the
// project entirely) must not reach the card, and the entitled service must be
// derived with no MCP connect — diagnoseDoc's endpoint here is unreachable.
func TestEntitlementsUsesScopeGate(t *testing.T) {
	foreign := diagnoseDoc("http://127.0.0.1:1/mcp")
	foreign.Metadata = &capability.Metadata{Namespace: "other-project"}
	foreign.Spec.ServiceRef = capability.Ref{Name: "otherco"}
	foreign.Spec.ServiceName = "streaming.otherco.example"

	conv := New(Deps{
		Source: fakeSource{docs: []capability.CapabilityDocument{
			foreign,
			diagnoseDoc("http://127.0.0.1:1/mcp"),
		}},
	})

	ents := conv.Entitlements(context.Background(), "demo-project")
	if len(ents) != 1 {
		t.Fatalf("entitlements = %d, want 1 (other-project's document must be dropped): %+v", len(ents), ents)
	}
	if ents[0].ServiceRef != "streamco" || ents[0].ServiceName != "streaming.streamco.example" {
		t.Fatalf("unexpected entitlement: %+v", ents[0])
	}
	if len(ents[0].ToolNames) != 1 || ents[0].ToolNames[0] != "streamco__pipeline_diagnose" {
		t.Fatalf("tools = %v, want the declared include list namespaced", ents[0].ToolNames)
	}
}

// TestEntitlementsNoSource degrades to nothing rather than failing, matching
// loadDocuments.
func TestEntitlementsNoSource(t *testing.T) {
	if ents := New(Deps{}).Entitlements(context.Background(), "demo-project"); len(ents) != 0 {
		t.Fatalf("entitlements = %+v, want none", ents)
	}
}
