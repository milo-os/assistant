package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore"
)

// Platform capabilities: what every project gets whether or not anything
// entitled it, because the service publishing it is the platform itself.

// locationsDoc is a platform-shaped document — no catalog fields, one MCP
// server, one tool.
func locationsDoc(mutate func(*CapabilityDocument)) CapabilityDocument {
	doc := CapabilityDocument{
		Metadata: &Metadata{Name: "locations-platform-binding"},
		Spec: CapabilitySpec{
			ServiceRef:  Ref{Name: "locations"},
			ServiceName: "locations.miloapis.com",
			Tools: &Tools{MCPServers: []MCPServer{{
				Name:         "locations",
				Endpoint:     "http://locations/mcp",
				ToolSelector: ToolSelector{Include: []string{"locations_list"}},
			}}},
		},
	}
	if mutate != nil {
		mutate(&doc)
	}
	return doc
}

// platformDoc is locationsDoc as the platform source would hand it over.
func platformDoc(mutate func(*CapabilityDocument)) CapabilityDocument {
	doc := locationsDoc(mutate)
	markPlatform(&doc)
	return doc
}

// answering builds a session whose tools report which server answered, so a
// tie between two documents can be settled by reading the output.
func answering(server string, names ...string) *fakeSession {
	tools := map[string]agentcore.Tool{}
	for _, n := range names {
		name, from := n, server
		tools[name] = fakeTool{name: name, exec: func(json.RawMessage) (string, error) {
			return `{"answered_by":"` + from + `"}`, nil
		}}
	}
	return &fakeSession{tools: tools}
}

// ── Composition ───────────────────────────────────────────────

// The whole point: a project that nothing entitled to the locations service
// still gets its tools, even when the document names some other namespace
// outright — markPlatform drops it before ScopeDocuments can read it as
// another tenant's.
func TestPlatformDocumentsComposeForAProjectTheyDoNotName(t *testing.T) {
	var buf bytes.Buffer
	doc := platformDoc(func(d *CapabilityDocument) {
		d.Metadata = &Metadata{Name: "locations-platform-binding", Namespace: "milo-system"}
		markPlatform(d)
	})
	composed, err := Compose(context.Background(), []CapabilityDocument{doc}, ComposeOptions{
		connect:         connectorFor(map[string]*fakeSession{"http://locations/mcp": answering("platform", "locations_list")}),
		ExpectedProject: "somebody-elses-project",
		Logger:          testLogger(&buf),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = composed.Close() }()

	if composed.Tools["locations__locations_list"] == nil {
		t.Fatalf("a platform document must compose for every project; got %v", keys(composed.Tools))
	}
	if strings.Contains(buf.String(), "scope.rejected") {
		t.Fatalf("a platform document must not be scope-rejected; logs:\n%s", buf.String())
	}
}

// Two documents, one tool name. Registration is first-wins and the platform
// source puts its documents first, so a project-scoped document can never
// shadow the platform's own record with one of its own.
func TestPlatformDocumentsWinAToolNameTie(t *testing.T) {
	var buf bytes.Buffer
	platform := platformDoc(nil)
	impostor := locationsDoc(func(d *CapabilityDocument) {
		d.Metadata = &Metadata{Name: "impostor", Namespace: "demo-project"}
		d.Spec.ServiceName = "locations.impostor.example"
		d.Spec.Tools.MCPServers[0].Endpoint = "http://impostor/mcp"
	})

	composed, err := Compose(context.Background(), []CapabilityDocument{platform, impostor}, ComposeOptions{
		connect: connectorFor(map[string]*fakeSession{
			"http://locations/mcp": answering("platform", "locations_list"),
			"http://impostor/mcp":  answering("impostor", "locations_list"),
		}),
		ExpectedProject: "demo-project",
		Logger:          testLogger(&buf),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = composed.Close() }()

	out, err := composed.Tools["locations__locations_list"].Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"answered_by":"platform"`) {
		t.Fatalf("the platform document must win the tie; answer was %s", out)
	}
	if !strings.Contains(buf.String(), "tool_collision") {
		t.Fatalf("the losing registration must be logged; logs:\n%s", buf.String())
	}
}

// ── Metering ──────────────────────────────────────────────────

// A platform capability is composed into a project regardless of entitlement,
// so the project never chose it and must not be billed a tool invocation for
// it. No event on any meter — not a different one.
func TestPlatformToolsNeverMeter(t *testing.T) {
	var invocations []ProviderToolInvocation
	composed, err := Compose(context.Background(), []CapabilityDocument{platformDoc(nil)}, ComposeOptions{
		connect:          connectorFor(map[string]*fakeSession{"http://locations/mcp": answering("platform", "locations_list")}),
		OnToolInvocation: func(inv ProviderToolInvocation) { invocations = append(invocations, inv) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = composed.Close() }()

	tool := composed.Tools["locations__locations_list"]
	if _, ok := tool.(*meteredTool); ok {
		t.Fatal("a platform tool must not carry the metering wrapper at all")
	}
	for range 3 {
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if len(invocations) != 0 {
		t.Fatalf("a platform tool fired %d billing events, want 0: %+v", len(invocations), invocations)
	}
}

// The rule is narrow: everything a project WAS entitled to still meters, so
// this cannot quietly turn billing off.
func TestProjectToolsStillMeterAlongsideAPlatformOne(t *testing.T) {
	var invocations []ProviderToolInvocation
	docs := []CapabilityDocument{
		platformDoc(nil),
		streamcoDoc(func(d *CapabilityDocument) { d.Spec.Knowledge = nil }),
	}
	composed, err := Compose(context.Background(), docs, ComposeOptions{
		connect: connectorFor(map[string]*fakeSession{
			"http://locations/mcp": answering("platform", "locations_list"),
			"http://provider/mcp":  answering("streamco", "streams_list", "pipeline_diagnose"),
		}),
		OnToolInvocation: func(inv ProviderToolInvocation) { invocations = append(invocations, inv) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = composed.Close() }()

	_, _ = composed.Tools["locations__locations_list"].Execute(context.Background(), json.RawMessage(`{}`))
	_, _ = composed.Tools["streamco__streams_list"].Execute(context.Background(), json.RawMessage(`{}`))

	if len(invocations) != 1 || invocations[0].ServiceName != "streaming.streamco.example" {
		t.Fatalf("only the entitled service should meter; got %+v", invocations)
	}
}

// The env-var seam: an operator can take any named service off the meter.
func TestUnmeteredServicesTakesANamedServiceOffTheMeter(t *testing.T) {
	var invocations []ProviderToolInvocation
	doc := streamcoDoc(func(d *CapabilityDocument) { d.Spec.Knowledge = nil })
	composed, err := Compose(context.Background(), []CapabilityDocument{doc}, ComposeOptions{
		connect:           connectorFor(map[string]*fakeSession{"http://provider/mcp": answering("streamco", "streams_list", "pipeline_diagnose")}),
		OnToolInvocation:  func(inv ProviderToolInvocation) { invocations = append(invocations, inv) },
		UnmeteredServices: []string{" streaming.streamco.example ", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = composed.Close() }()

	_, _ = composed.Tools["streamco__streams_list"].Execute(context.Background(), json.RawMessage(`{}`))
	if len(invocations) != 0 {
		t.Fatalf("CAPABILITY_UNMETERED_SERVICES did not silence the meter: %+v", invocations)
	}
}

// Naming a platform service in the env var cannot put it BACK on the meter:
// the set is a union, because a project that was given a capability must not
// then be billed for it.
func TestUnmeteredServicesCannotRemeterAPlatformService(t *testing.T) {
	unmetered := unmeteredServices([]CapabilityDocument{platformDoc(nil)}, []string{"something.else.example"})
	if !unmetered["locations.miloapis.com"] {
		t.Fatal("a platform service must stay unmetered whatever the env var says")
	}
	if !unmetered["something.else.example"] {
		t.Fatal("the env var must extend the set")
	}
}

// ── The source ────────────────────────────────────────────────

type staticSource struct {
	docs []CapabilityDocument
	err  error
	saw  []string
}

func (s *staticSource) Documents(_ context.Context, projectName string) ([]CapabilityDocument, error) {
	s.saw = append(s.saw, projectName)
	return s.docs, s.err
}

func TestPlatformSourceServesPlatformDocumentsFirst(t *testing.T) {
	platform := &staticSource{docs: []CapabilityDocument{locationsDoc(nil)}}
	project := &staticSource{docs: []CapabilityDocument{streamcoDoc(nil)}}

	docs, err := NewPlatformSource(platform, project, nil).Documents(context.Background(), "demo-project")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("want both documents, got %d", len(docs))
	}
	if docs[0].Spec.ServiceName != "locations.miloapis.com" {
		t.Fatalf("platform documents must come first; got %s", docs[0].Spec.ServiceName)
	}
	if !docs[0].IsPlatform() {
		t.Fatal("a document from the platform source must be marked platform")
	}
	if docs[1].IsPlatform() {
		t.Fatal("a project document must not be marked platform")
	}
	if project.saw[0] != "demo-project" {
		t.Fatalf("the project source must still be asked about the calling project; saw %v", project.saw)
	}
}

// A source that was not built with the platform constructor still cannot leak
// a project-scoped or a metered platform document.
func TestPlatformSourceStripsANamespaceItWasHandedAnyway(t *testing.T) {
	platform := &staticSource{docs: []CapabilityDocument{locationsDoc(func(d *CapabilityDocument) {
		d.Metadata = &Metadata{Name: "locations", Namespace: "milo-system"}
	})}}

	docs, err := NewPlatformSource(platform, nil, nil).Documents(context.Background(), "demo-project")
	if err != nil {
		t.Fatal(err)
	}
	if docs[0].Metadata.Namespace != "" {
		t.Fatalf("namespace = %q, want cleared", docs[0].Metadata.Namespace)
	}
	if !docs[0].IsPlatform() {
		t.Fatal("want marked platform")
	}
}

// Neither half may take the other down: an unreachable platform provider must
// not cost a project what it is entitled to, and vice versa.
func TestPlatformSourceDegradesEachHalfIndependently(t *testing.T) {
	t.Run("platform fails", func(t *testing.T) {
		var buf bytes.Buffer
		src := NewPlatformSource(
			&staticSource{err: errors.New("provider down")},
			&staticSource{docs: []CapabilityDocument{streamcoDoc(nil)}},
			testLogger(&buf))
		docs, err := src.Documents(context.Background(), "demo-project")
		if err != nil || len(docs) != 1 {
			t.Fatalf("docs = %d, err = %v", len(docs), err)
		}
		if !strings.Contains(buf.String(), "capability.platform.load_failed") {
			t.Fatalf("the failure must be logged; logs:\n%s", buf.String())
		}
	})
	t.Run("project fails", func(t *testing.T) {
		var buf bytes.Buffer
		src := NewPlatformSource(
			&staticSource{docs: []CapabilityDocument{locationsDoc(nil)}},
			&staticSource{err: errors.New("catalog down")},
			testLogger(&buf))
		docs, err := src.Documents(context.Background(), "demo-project")
		if err != nil || len(docs) != 1 {
			t.Fatalf("docs = %d, err = %v", len(docs), err)
		}
		if !strings.Contains(buf.String(), "capability.project.load_failed") {
			t.Fatalf("the failure must be logged; logs:\n%s", buf.String())
		}
	})
}

// ── Parsing ───────────────────────────────────────────────────

const platformDocJSON = `{
  "metadata": { "name": "locations-platform-binding" },
  "spec": {
    "serviceRef": { "name": "locations" },
    "serviceName": "locations.miloapis.com",
    "tools": {
      "mcpServers": [{
        "name": "locations",
        "endpoint": "http://locations/mcp",
        "toolSelector": { "include": ["locations_list"] }
      }]
    }
  }
}`

// serviceAgentRef and configurationVersion are catalog concepts. A platform
// service never passes through the catalog and has no honest value for
// either, so requiring one would only get a placeholder typed into a ConfigMap.
func TestPlatformDocumentsDoNotNeedTheCatalogOnlyFields(t *testing.T) {
	docs, err := ParsePlatformDocuments([]byte("["+platformDocJSON+"]"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 document, got %d", len(docs))
	}
	if !docs[0].IsPlatform() {
		t.Fatal("a parsed platform document must be marked platform")
	}

	// The per-project path is unchanged: the catalog fills those fields in on
	// every document it projects, so a missing one there is a real defect.
	var skipped []string
	strict, err := ParseDocuments([]byte("["+platformDocJSON+"]"), func(_ int, skipErr error) {
		skipped = append(skipped, skipErr.Error())
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(strict) != 0 {
		t.Fatal("the per-project path must still require the catalog fields")
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "serviceAgentRef") {
		t.Fatalf("skips = %v", skipped)
	}
}

// serviceName is the metering dimension and the unmetered-service key, so it
// stays required on both paths.
func TestAPlatformDocumentStillNeedsAServiceName(t *testing.T) {
	docs, err := ParsePlatformDocuments([]byte(`[{"spec":{"serviceRef":{"name":"locations"}}}]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Fatal("a document with no serviceName must be skipped even on the platform path")
	}
}

// A provider publishes the per-project documents, so "platform" must not be
// something a document can say about itself: one that could would compose
// itself into every tenant and stop being billed while doing it.
func TestAProviderCannotDeclareItselfPlatform(t *testing.T) {
	raw := `[{
	  "platform": true,
	  "metadata": { "name": "impostor", "namespace": "demo-project", "platform": true },
	  "spec": {
	    "platform": true,
	    "serviceRef": { "name": "impostor" },
	    "serviceName": "impostor.example",
	    "serviceAgentRef": { "name": "impostor-agent" },
	    "configurationVersion": "v1"
	  }
	}]`
	docs, err := ParseDocuments([]byte(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 document, got %d", len(docs))
	}
	if docs[0].IsPlatform() {
		t.Fatal("a per-project document declared itself platform")
	}
	if docs[0].Metadata.Namespace != "demo-project" {
		t.Fatal("a per-project document must keep the namespace that scopes it")
	}
	if unmeteredServices(docs, nil)["impostor.example"] {
		t.Fatal("a per-project document talked its way off the meter")
	}
}

// ── Fixture wiring ────────────────────────────────────────────

func TestPlatformFixtureSourceMarksAndClears(t *testing.T) {
	path := writeFixture(t, `[`+strings.Replace(platformDocJSON,
		`"metadata": { "name": "locations-platform-binding" }`,
		`"metadata": { "name": "locations-platform-binding", "namespace": "milo-system" }`, 1)+`]`)

	docs, err := NewPlatformFixtureSource(path, nil).Documents(context.Background(), "demo-project")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || !docs[0].IsPlatform() || docs[0].Metadata.Namespace != "" {
		t.Fatalf("docs = %+v", docs)
	}

	// The ordinary constructor reads the same file the ordinary way.
	plain, err := NewFixtureSource(path, nil).Documents(context.Background(), "demo-project")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 0 {
		t.Fatal("the per-project fixture source must still enforce the catalog fields")
	}
}

// ── The agent card ────────────────────────────────────────────

// A card is derived from the same documents the next turn composes, so a
// platform service has to appear on every project's card. One that advertised
// only entitled services would under-report what the assistant can do.
func TestPlatformServicesAppearOnEveryProjectsCard(t *testing.T) {
	docs := []CapabilityDocument{platformDoc(nil)}
	entitlements := Entitlements(ScopeDocuments(docs, "a-project-with-no-entitlements", nil))
	if len(entitlements) != 1 {
		t.Fatalf("want the platform service on the card, got %+v", entitlements)
	}
	if entitlements[0].ServiceName != "locations.miloapis.com" {
		t.Fatalf("service = %q", entitlements[0].ServiceName)
	}
	if len(entitlements[0].ToolNames) != 1 || entitlements[0].ToolNames[0] != "locations__locations_list" {
		t.Fatalf("a platform provider's tools are namespaced like any other; got %v", entitlements[0].ToolNames)
	}
}
