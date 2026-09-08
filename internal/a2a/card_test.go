package a2a

import (
	"encoding/json"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"reflect"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{PublicBaseURL: "http://assistant.test"}
}

func TestBuildAgentCard_PublicCardAdvertisesExtendedCardOnly(t *testing.T) {
	card := BuildAgentCard(testConfig())

	if !card.Capabilities.ExtendedAgentCard {
		t.Error("public card must advertise extendedAgentCard, or clients short-circuit the method")
	}
	if !card.Capabilities.Streaming {
		t.Error("streaming capability should stay true")
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "project-assistant" {
		t.Fatalf("public skills = %+v, want only project-assistant", card.Skills)
	}
}

// The public card is unauthenticated, so it must name no service, tool or MCP
// endpoint — that disclosure line only moves on the extended card.
func TestBuildAgentCard_PublicCardLeaksNoEntitlement(t *testing.T) {
	cfg := testConfig()
	// Mixed case throughout, because a case-sensitive probe would miss a real
	// provider name that happens to be capitalized differently.
	ent := capability.ServiceEntitlement{
		ID:              "streamco",
		ServiceRef:      "streamco",
		ServiceName:     "Streaming.StreamCo.Example",
		ToolNames:       []string{"streamco__Streams_List"},
		SkillNames:      []string{"streamco__Lag-Triage"},
		KnowledgeTitles: []string{"StreamCo Runbook"},
		MCPEndpoints:    []string{"https://mcp.StreamCo.test:8443/mcp"},
	}
	leaks := []string{
		ent.ID, ent.ServiceName, ent.ToolNames[0],
		ent.SkillNames[0], ent.KnowledgeTitles[0], ent.MCPEndpoints[0],
	}

	public := strings.ToLower(marshalCard(t, BuildAgentCard(cfg)))
	for _, leak := range leaks {
		if strings.Contains(public, strings.ToLower(leak)) {
			t.Errorf("public card leaks %q: %s", leak, public)
		}
	}

	// Keep the check above from going vacuous: every probe must be a string that
	// really would show up if the public card ever started disclosing it.
	extended := strings.ToLower(marshalCard(t, BuildExtendedAgentCard(cfg, []capability.ServiceEntitlement{ent})))
	for _, leak := range leaks {
		if !strings.Contains(extended, strings.ToLower(leak)) {
			t.Errorf("probe %q never appears even on the extended card, so it proves nothing", leak)
		}
	}
}

func marshalCard(t *testing.T, card *a2a.AgentCard) string {
	t.Helper()
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBuildExtendedAgentCard_NoEntitlementsKeepsGenericSkill(t *testing.T) {
	card := BuildExtendedAgentCard(testConfig(), nil)

	if len(card.Skills) != 1 || card.Skills[0].ID != "project-assistant" {
		t.Fatalf("skills = %+v, want only project-assistant", card.Skills)
	}
}

func TestBuildExtendedAgentCard_OneSkillPerService(t *testing.T) {
	ents := []capability.ServiceEntitlement{
		{
			ID:                "streamco",
			ServiceRef:        "streamco",
			ServiceName:       "streaming.streamco.example",
			ToolNames:         []string{"streamco__streams_list", "streamco__pipeline_diagnose"},
			MCPEndpoints:      []string{"http://mcp.streamco.test/mcp"},
			SkillNames:        []string{"streamco__lag-triage"},
			SkillDescriptions: []string{"Diagnose consumer lag"},
		},
		{
			ID:              "acmedb",
			ServiceRef:      "acmedb",
			ServiceName:     "db.acme.example",
			KnowledgeTitles: []string{"Runbook"},
		},
	}

	card := BuildExtendedAgentCard(testConfig(), ents)

	if len(card.Skills) != 3 {
		t.Fatalf("skills = %d, want generic + 2 services: %+v", len(card.Skills), card.Skills)
	}
	if card.Skills[0].ID != "project-assistant" {
		t.Errorf("generic skill must stay first, got %q", card.Skills[0].ID)
	}
	streamco := card.Skills[1]
	if streamco.ID != "streamco" || streamco.Name != "streaming.streamco.example" {
		t.Errorf("service skill = %q/%q", streamco.ID, streamco.Name)
	}
	for _, want := range []string{"datum", "provider-service", "streamco", "streamco__streams_list", "streamco__pipeline_diagnose"} {
		if !hasTag(streamco.Tags, want) {
			t.Errorf("tags %v missing %q", streamco.Tags, want)
		}
	}
	// Endpoints are addresses, not keywords: description only.
	if hasTag(streamco.Tags, "http://mcp.streamco.test/mcp") {
		t.Errorf("MCP endpoint must not be a tag: %v", streamco.Tags)
	}
	for _, want := range []string{"streaming.streamco.example", "streamco__lag-triage", "http://mcp.streamco.test/mcp"} {
		if !strings.Contains(streamco.Description, want) {
			t.Errorf("description %q missing %q", streamco.Description, want)
		}
	}
	if len(streamco.Examples) != 1 || streamco.Examples[0] != "Diagnose consumer lag" {
		t.Errorf("examples = %v, want the published skill description", streamco.Examples)
	}
	if len(streamco.InputModes) != 1 || streamco.InputModes[0] != "text/plain" {
		t.Errorf("input modes = %v", streamco.InputModes)
	}

	// A service with no published skills invents no examples.
	if got := card.Skills[2]; got.ID != "acmedb" || len(got.Examples) != 0 {
		t.Errorf("acmedb skill = %+v, want no examples", got)
	}
}

// The extended card is the public card plus skills: everything a client already
// resolved (transport, auth, identity) must survive.
func TestBuildExtendedAgentCard_RetainsPublicCardIdentity(t *testing.T) {
	cfg := testConfig()
	pub := BuildAgentCard(cfg)
	ext := BuildExtendedAgentCard(cfg, []capability.ServiceEntitlement{{ID: "streamco", ServiceRef: "streamco", ServiceName: "streaming.streamco.example"}})

	if ext.Version != pub.Version || ext.Name != pub.Name {
		t.Errorf("identity drifted: %q/%q vs %q/%q", ext.Name, ext.Version, pub.Name, pub.Version)
	}
	if len(ext.SupportedInterfaces) != 1 || ext.SupportedInterfaces[0].URL != pub.SupportedInterfaces[0].URL {
		t.Errorf("interfaces = %+v", ext.SupportedInterfaces)
	}
	if _, ok := ext.SecuritySchemes[BearerSchemeName]; !ok {
		t.Errorf("security schemes = %+v", ext.SecuritySchemes)
	}
	if ext.Provider == nil || ext.Provider.Org != pub.Provider.Org {
		t.Errorf("provider = %+v", ext.Provider)
	}
	if !reflect.DeepEqual(ext.Capabilities, pub.Capabilities) {
		t.Errorf("capabilities = %+v, want %+v", ext.Capabilities, pub.Capabilities)
	}
}

// Building the public card twice must not see skills appended by a prior
// extended-card build.
func TestBuildExtendedAgentCard_DoesNotMutatePublicCard(t *testing.T) {
	cfg := testConfig()
	BuildExtendedAgentCard(cfg, []capability.ServiceEntitlement{{ID: "streamco", ServiceRef: "streamco", ServiceName: "streaming.streamco.example"}})

	if got := len(BuildAgentCard(cfg).Skills); got != 1 {
		t.Errorf("public skills = %d, want 1", got)
	}
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
