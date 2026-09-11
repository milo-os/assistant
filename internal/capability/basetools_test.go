package capability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/basetools"
	"github.com/milo-os/assistant/internal/projectapi"
)

// The base platform tools are not one provider's contribution: they are what
// every project has, so composition adds them without a capability document
// asking for it.

func platformClient(t *testing.T) (*projectapi.Client, *[]string) {
	t.Helper()
	var authorizations []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resources":[]}`))
	}))
	t.Cleanup(srv.Close)

	client, err := projectapi.New(projectapi.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("projectapi.New: %v", err)
	}
	return client, &authorizations
}

func TestComposeAddsTheBaseToolsToEveryProject(t *testing.T) {
	client, _ := platformClient(t)

	composed, err := Compose(context.Background(), nil, ComposeOptions{
		PlatformAPI:     client,
		ExpectedProject: "demo-project",
		Caller:          CallerIdentity{BearerToken: "caller-token"},
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer func() { _ = composed.Close() }()

	for _, name := range []string{
		basetools.ResourcesListToolName,
		basetools.ResourcesGetToolName,
		basetools.SchemaGetToolName,
		basetools.LocationsListToolName,
		basetools.QuotaGetToolName,
	} {
		if _, exists := composed.Tools[name]; !exists {
			t.Fatalf("%s was not composed for a project with no capability documents at all", name)
		}
	}
	if !strings.Contains(composed.SystemPromptAddendum, basetools.ResourcesListToolName) {
		t.Fatalf("the prompt does not mention the base tools:\n%s", composed.SystemPromptAddendum)
	}
}

// With nobody to act as, there is nothing to compose. Reading as the service
// instead is the one behaviour this must never fall back to.
func TestComposeSkipsTheBaseToolsWithNoCaller(t *testing.T) {
	client, _ := platformClient(t)

	for _, tc := range []struct {
		name    string
		project string
		token   string
	}{
		{name: "no credential", project: "demo-project"},
		{name: "no project", token: "caller-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			composed, err := Compose(context.Background(), nil, ComposeOptions{
				PlatformAPI:     client,
				ExpectedProject: tc.project,
				Caller:          CallerIdentity{BearerToken: tc.token},
			})
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			defer func() { _ = composed.Close() }()

			if _, exists := composed.Tools[basetools.ResourcesListToolName]; exists {
				t.Fatal("the base tools were composed with nobody to act as")
			}
			if strings.Contains(composed.SystemPromptAddendum, basetools.ResourcesListToolName) {
				t.Fatal("the prompt advertises tools that are not there")
			}
		})
	}
}

func TestComposeWithoutAPlatformAPIComposesNoBaseTools(t *testing.T) {
	composed, err := Compose(context.Background(), nil, ComposeOptions{
		ExpectedProject: "demo-project",
		Caller:          CallerIdentity{BearerToken: "caller-token"},
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer func() { _ = composed.Close() }()

	if len(composed.Tools) != 0 {
		t.Fatalf("tools = %v, want none", composed.Tools)
	}
}

// The credential the composed tools carry is the caller's, taken from the turn
// rather than held anywhere.
func TestComposedBaseToolsActAsTheCaller(t *testing.T) {
	client, authorizations := platformClient(t)

	composed, err := Compose(context.Background(), nil, ComposeOptions{
		PlatformAPI:     client,
		ExpectedProject: "demo-project",
		Caller:          CallerIdentity{BearerToken: "caller-token"},
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer func() { _ = composed.Close() }()

	_, _ = composed.Tools[basetools.ResourcesListToolName].Execute(context.Background(),
		[]byte(`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload"}`))

	if len(*authorizations) == 0 {
		t.Fatal("the tool made no request")
	}
	for _, got := range *authorizations {
		if got != "Bearer caller-token" {
			t.Fatalf("request carried %q, want the caller's own credential", got)
		}
	}
}

// A provider tool always carries its service prefix, so it can never take a
// base tool's name. The check is here rather than left to chance.
func TestProviderToolsCannotShadowABaseTool(t *testing.T) {
	client, _ := platformClient(t)

	doc := streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.Tools.MCPServers[0].ToolSelector.Include = []string{basetools.ResourcesListToolName}
	})
	session := newFakeSession(basetools.ResourcesListToolName)

	composed, err := Compose(context.Background(), []CapabilityDocument{doc}, ComposeOptions{
		PlatformAPI:     client,
		ExpectedProject: "demo-project",
		Caller:          CallerIdentity{BearerToken: "caller-token"},
		connect:         connectorFor(map[string]*fakeSession{"http://provider/mcp": session}),
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer func() { _ = composed.Close() }()

	tool, exists := composed.Tools[basetools.ResourcesListToolName]
	if !exists {
		t.Fatal("the base tool is missing")
	}
	if !strings.Contains(tool.Definition().Description, "Read-only.") {
		t.Fatalf("%s is not the base tool: %q", basetools.ResourcesListToolName, tool.Definition().Description)
	}
	// The provider's own copy of the name is still there, namespaced, and is
	// a different tool.
	namespaced := NamespaceToolName("streamco", basetools.ResourcesListToolName)
	if _, exists := composed.Tools[namespaced]; !exists {
		t.Fatalf("the provider tool is missing under %q", namespaced)
	}
}
