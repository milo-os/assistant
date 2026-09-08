package capability

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// fixtureDocs loads the shipped capability-document fixture — the same file
// the local slice serves — so the card path is pinned against real input.
func fixtureDocs(t *testing.T) []CapabilityDocument {
	t.Helper()
	raw, err := os.ReadFile("../../fixtures/capability-documents.json")
	if err != nil {
		t.Fatal(err)
	}
	docs, err := ParseDocuments(raw, func(i int, err error) { t.Fatalf("fixture document %d invalid: %v", i, err) })
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

func TestEntitlements_NoDocuments(t *testing.T) {
	if got := Entitlements(nil); len(got) != 0 {
		t.Fatalf("want no entitlements, got %+v", got)
	}
}

func TestEntitlements_FromFixture(t *testing.T) {
	ents := Entitlements(fixtureDocs(t))
	if len(ents) != 1 {
		t.Fatalf("want 1 entitlement, got %+v", ents)
	}
	ent := ents[0]
	if ent.ServiceRef != "streamco" || ent.ServiceName != "streaming.streamco.example" {
		t.Fatalf("service = %q / %q", ent.ServiceRef, ent.ServiceName)
	}
	if want := []string{"streamco__streams_list", "streamco__pipeline_diagnose"}; !reflect.DeepEqual(ent.ToolNames, want) {
		t.Fatalf("tools = %v, want %v", ent.ToolNames, want)
	}
	if want := []string{"streamco__lag-triage"}; !reflect.DeepEqual(ent.SkillNames, want) {
		t.Fatalf("skills = %v, want %v", ent.SkillNames, want)
	}
	if len(ent.SkillDescriptions) != 1 || !strings.Contains(ent.SkillDescriptions[0], "consumer lag") {
		t.Fatalf("skill descriptions = %v", ent.SkillDescriptions)
	}
	if want := []string{"http://127.0.0.1:7810/mcp"}; !reflect.DeepEqual(ent.MCPEndpoints, want) {
		t.Fatalf("endpoints = %v, want %v", ent.MCPEndpoints, want)
	}
}

func TestEntitlements_DuplicateServiceRefMergesToMatchCompose(t *testing.T) {
	// Compose (connectTools/collectSkills) iterates every document and never
	// de-duplicates by serviceRef, so two documents sharing one MUST merge here.
	// Dropping the second would advertise less than the next turn actually runs
	// — a router would conclude a tool is unavailable when it is not. The card
	// describes what Compose will do; it is not an access-control boundary, so
	// a document that should not be trusted has to be rejected upstream, at the
	// Source or the ScopeDocuments gate.
	first := streamcoDoc(func(d *CapabilityDocument) { d.Spec.Knowledge = nil })
	second := streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.Knowledge = nil
		d.Spec.ServiceName = "second.streamco.example"
		d.Spec.Tools = &Tools{MCPServers: []MCPServer{{
			Name: "streamco-us", Endpoint: "http://provider-us/mcp",
			ToolSelector: ToolSelector{Include: []string{"failover_status"}},
		}}}
	})
	docs := []CapabilityDocument{first, second}

	composed, err := Compose(context.Background(), docs, ComposeOptions{
		connect: connectorFor(map[string]*fakeSession{
			"http://provider/mcp":    newFakeSession("streams_list", "pipeline_diagnose"),
			"http://provider-us/mcp": newFakeSession("failover_status"),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	ents := Entitlements(docs)
	if len(ents) != 1 {
		t.Fatalf("want 1 entitlement for one serviceRef, got %+v", ents)
	}
	// The first document still names the service; only its surface grows.
	if ents[0].ServiceName != "streaming.streamco.example" {
		t.Fatalf("second registration won the name: %+v", ents[0])
	}

	for _, want := range []string{"streamco__streams_list", "streamco__pipeline_diagnose", "streamco-us__failover_status"} {
		if _, ok := composed.Tools[want]; !ok {
			t.Fatalf("precondition: Compose did not register %q", want)
		}
		if !slices.Contains(ents[0].ToolNames, want) {
			t.Errorf("card under-advertises %q, which a turn would run: %v", want, ents[0].ToolNames)
		}
	}
}

func TestEntitlements_IDIsSanitized(t *testing.T) {
	// serviceRef is provider-supplied and only validated as non-empty, so the
	// identifier a card publishes is sanitized the same way tool names are
	// rather than passed through verbatim.
	doc := streamcoDoc(func(d *CapabilityDocument) { d.Spec.ServiceRef.Name = "stream co/eu" })

	ents := Entitlements([]CapabilityDocument{doc})
	if len(ents) != 1 {
		t.Fatalf("want 1 entitlement, got %+v", ents)
	}
	if want := SanitizeName("stream co/eu"); ents[0].ID != want {
		t.Errorf("ID = %q, want %q", ents[0].ID, want)
	}
	if ents[0].ServiceRef != "stream co/eu" {
		t.Errorf("ServiceRef should stay verbatim, got %q", ents[0].ServiceRef)
	}
	// The ID must share the prefix the document's own skill names carry.
	for _, name := range ents[0].SkillNames {
		if !strings.HasPrefix(name, ents[0].ID+ToolNamespaceSeparator) {
			t.Errorf("skill %q does not use the advertised ID %q", name, ents[0].ID)
		}
	}
}

func TestEntitlements_NamesUseTheSharedSanitizer(t *testing.T) {
	doc := streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.Knowledge = nil
		d.Spec.ServiceRef = Ref{Name: "stream.co/eu"}
		d.Spec.Tools.MCPServers[0].Name = "stream.co/eu"
		d.Spec.Tools.MCPServers[0].ToolSelector.Include = []string{"streams:list"}
		d.Spec.Skills = []Skill{{Name: "lag triage", Description: "d", Source: "http://provider/s.md"}}
	})

	ents := Entitlements([]CapabilityDocument{doc})
	if len(ents) != 1 {
		t.Fatalf("want 1 entitlement, got %+v", ents)
	}
	if want := NamespaceToolName("stream.co/eu", "streams:list"); ents[0].ToolNames[0] != want {
		t.Fatalf("tool = %q, want %q", ents[0].ToolNames[0], want)
	}
	if want := NamespaceToolName("stream.co/eu", "lag triage"); ents[0].SkillNames[0] != want {
		t.Fatalf("skill = %q, want %q", ents[0].SkillNames[0], want)
	}
}

func TestEntitlements_SortedByServiceRef(t *testing.T) {
	streamco := streamcoDoc(func(d *CapabilityDocument) { d.Spec.Knowledge = nil })
	acme := streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.Knowledge = nil
		d.Spec.ServiceRef = Ref{Name: "acme"}
		d.Spec.ServiceName = "dns.acme.example"
		d.Spec.Tools = &Tools{MCPServers: []MCPServer{{
			Name: "acme", Endpoint: "http://acme/mcp", ToolSelector: ToolSelector{Include: []string{"zones_list"}},
		}}}
	})

	for _, docs := range [][]CapabilityDocument{{streamco, acme}, {acme, streamco}} {
		ents := Entitlements(docs)
		refs := []string{}
		for _, e := range ents {
			refs = append(refs, e.ServiceRef)
		}
		if !reflect.DeepEqual(refs, []string{"acme", "streamco"}) {
			t.Fatalf("refs = %v, want sorted [acme streamco]", refs)
		}
	}
}

func TestEntitlements_KnowledgeOnlyIncludedEmptySkipped(t *testing.T) {
	knowledgeOnly := streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.Tools = nil
		d.Spec.Knowledge = &Knowledge{Sources: []KnowledgeSource{
			{Type: KnowledgeLLMDocs, Title: "StreamCo overview", URL: "http://provider/llms-full.txt"},
			{Type: KnowledgeRunbook, URL: "http://provider/runbook.md"}, // untitled => URL
		}}
	})
	empty := streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.ServiceRef = Ref{Name: "hollow"}
		d.Spec.ServiceName = "hollow.example"
		d.Spec.Knowledge = nil
		d.Spec.Tools = nil
	})

	ents := Entitlements([]CapabilityDocument{knowledgeOnly, empty})
	if len(ents) != 1 || ents[0].ServiceRef != "streamco" {
		t.Fatalf("want only the knowledge-bearing document, got %+v", ents)
	}
	want := []string{"StreamCo overview", "http://provider/runbook.md"}
	if !reflect.DeepEqual(ents[0].KnowledgeTitles, want) {
		t.Fatalf("titles = %v, want %v", ents[0].KnowledgeTitles, want)
	}
}

// DRIFT PIN: the card and the next turn must describe the same services. The
// fixture composed against an MCP server exposing exactly its declared tools
// must yield the same service and tool names Entitlements derives from the
// documents alone — minus Patch's built-ins, which are never provider surface.
func TestEntitlements_MatchesComposeOnTheSameDocuments(t *testing.T) {
	docs := fixtureDocs(t)
	session := newFakeSession("streams_list", "pipeline_diagnose", "not_allow_listed")
	composed, err := Compose(context.Background(), docs, ComposeOptions{
		connect: connectorFor(map[string]*fakeSession{"http://127.0.0.1:7810/mcp": session}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	builtins := map[string]bool{LoadSkillToolName: true, RememberMemoryToolName: true, ForgetMemoryToolName: true}
	var composedTools []string
	for name := range composed.Tools {
		if builtins[name] || strings.HasPrefix(name, "report_capability_gap") {
			continue
		}
		composedTools = append(composedTools, name)
	}
	sort.Strings(composedTools)

	ents := Entitlements(docs)
	var declared []string
	for _, ent := range ents {
		declared = append(declared, ent.ToolNames...)
		for _, name := range ent.ToolNames {
			if builtins[name] || strings.HasPrefix(name, "report_capability_gap") {
				t.Fatalf("built-in %q advertised as provider surface", name)
			}
		}
	}
	sort.Strings(declared)

	if !reflect.DeepEqual(declared, composedTools) {
		t.Fatalf("card tools %v drifted from composed tools %v", declared, composedTools)
	}
	if len(ents) != 1 || ents[0].ServiceName != docs[0].Spec.ServiceName {
		t.Fatalf("service set drifted: %+v", ents)
	}
}

// SCOPE PIN: derivation happens downstream of the tenant gate. FixtureSource
// returns the whole file regardless of project, so skipping ScopeDocuments
// would put every project's services on every project's card.
func TestEntitlements_ScopeDocumentsDropsForeignProject(t *testing.T) {
	mine := streamcoDoc(func(d *CapabilityDocument) { d.Spec.Knowledge = nil })
	foreign := streamcoDoc(func(d *CapabilityDocument) {
		d.Metadata = &Metadata{Name: "leaked", Namespace: "other-project"}
		d.Spec.ServiceRef = Ref{Name: "evil"}
		d.Spec.ServiceName = "leak.evil.example"
		d.Spec.Knowledge = nil
		d.Spec.Tools = &Tools{MCPServers: []MCPServer{{
			Name: "evil", Endpoint: "http://evil/mcp", ToolSelector: ToolSelector{Include: []string{"exfiltrate"}},
		}}}
	})
	docs := []CapabilityDocument{mine, foreign}

	var buf bytes.Buffer
	ents := Entitlements(ScopeDocuments(docs, "demo-project", testLogger(&buf)))
	if len(ents) != 1 || ents[0].ServiceRef != "streamco" {
		t.Fatalf("foreign document reached the card: %+v", ents)
	}

	// Without the gate the same documents advertise the other project's service
	// — which is exactly why callers must pass ScopeDocuments output.
	if len(Entitlements(docs)) != 2 {
		t.Fatal("ungated derivation no longer sees both documents; the pin above proves nothing")
	}
}
