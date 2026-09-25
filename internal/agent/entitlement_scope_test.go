package agent

import (
	"context"
	"testing"

	"github.com/milo-os/assistant/internal/capability"
)

// The card path reaches ScopeDocuments without going through Compose, so it
// must apply exactly the gate the turn does — no looser and no stricter. A card
// stricter than the turn would hide a service the next turn happily composes; a
// card looser than the turn would advertise one the next turn refuses.
//
// The namespace-less case is the production case, not an edge: a
// CapabilityBinding is cluster-scoped inside its project's control plane, so
// every document the CRD source yields has an empty namespace. Dropping those
// would empty every agent card in the fleet.
func TestEntitlements_ScopeMatchesTheTurn(t *testing.T) {
	clusterScoped := diagnoseDoc("http://127.0.0.1:1/mcp")
	clusterScoped.Metadata = &capability.Metadata{Name: "acme-binding"} // no namespace

	conv := New(Deps{Source: fakeSource{docs: []capability.CapabilityDocument{clusterScoped}}})
	if ents := conv.Entitlements(context.Background(), "demo-project"); len(ents) != 1 {
		t.Fatalf("cluster-scoped document must be advertised, got %+v", ents)
	}

	// A document that DOES name a namespace, and names someone else's, is still
	// dropped: that is the fixture/HTTP payload shape, where the check is real.
	foreign := diagnoseDoc("http://127.0.0.1:1/mcp")
	foreign.Metadata = &capability.Metadata{Name: "leaked", Namespace: "other-tenant"}

	conv = New(Deps{Source: fakeSource{docs: []capability.CapabilityDocument{foreign}}})
	if ents := conv.Entitlements(context.Background(), "demo-project"); len(ents) != 0 {
		t.Fatalf("another project's document must never reach a card, got %+v", ents)
	}
}
