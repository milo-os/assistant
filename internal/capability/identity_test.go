package capability

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

const callerToken = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.caller-secret"

// recordingConnector captures the headers Compose decided each endpoint may
// receive, keyed by endpoint. A nil entry means "connected without identity".
func recordingConnector(seen map[string]map[string]string, session *fakeSession) mcpConnector {
	return func(_ context.Context, endpoint string, headers map[string]string) (mcpSession, error) {
		seen[endpoint] = headers
		return session, nil
	}
}

// gatewayDoc is streamcoDoc with its MCP endpoint on the (sanctioned) gateway
// host, which is where real capability documents point.
func gatewayDoc(endpoint string) CapabilityDocument {
	return streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.Knowledge = nil
		d.Spec.Tools.MCPServers[0].Endpoint = endpoint
	})
}

func composeWithConnector(t *testing.T, doc CapabilityDocument, opts ComposeOptions) map[string]map[string]string {
	t.Helper()
	seen := map[string]map[string]string{}
	opts.connect = recordingConnector(seen, newFakeSession("streams_list", "pipeline_diagnose"))
	composed, err := Compose(context.Background(), []CapabilityDocument{doc}, opts)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	t.Cleanup(func() { _ = composed.Close() })
	return seen
}

func TestIdentity_ForwardedToSanctionedEndpoint(t *testing.T) {
	const endpoint = "http://gateway.svc.cluster.local/mcp"
	seen := composeWithConnector(t, gatewayDoc(endpoint), ComposeOptions{
		Caller:               CallerIdentity{BearerToken: callerToken},
		ExpectedProject:      "demo-project",
		IdentityForwardHosts: []string{"gateway.svc.cluster.local"},
	})

	headers := seen[endpoint]
	if got := headers[AuthorizationHeader]; got != "Bearer "+callerToken {
		t.Fatalf("Authorization = %q, want the caller's bearer token", got)
	}
	if got := headers[ProjectHeader]; got != "demo-project" {
		t.Fatalf("%s = %q, want the authorized project", ProjectHeader, got)
	}
}

// The project header comes from the authorized request, never from the
// document — a document naming another tenant's project cannot re-aim a read.
func TestIdentity_ProjectComesFromTheRequestNotTheDocument(t *testing.T) {
	const endpoint = "http://gateway.svc.cluster.local/mcp"
	doc := gatewayDoc(endpoint)
	doc.Metadata.Namespace = "" // no namespace ⇒ scopeDocuments keeps it
	seen := composeWithConnector(t, doc, ComposeOptions{
		Caller:               CallerIdentity{BearerToken: callerToken},
		ExpectedProject:      "demo-project",
		IdentityForwardHosts: []string{"gateway.svc.cluster.local"},
	})
	if got := seen[endpoint][ProjectHeader]; got != "demo-project" {
		t.Fatalf("%s = %q, want demo-project", ProjectHeader, got)
	}
}

// The core defense: a capability document is provider-controlled data, so an
// endpoint it names gets no credential unless an operator sanctioned the host.
func TestIdentity_NotForwardedToUnsanctionedEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		hosts    []string
	}{
		{"harvester named by the document", "http://evil.example/mcp", []string{"gateway.svc.cluster.local"}},
		{"no sanctioned hosts at all (the default)", "http://gateway.svc.cluster.local/mcp", nil},
		{"suffix match must be on a label boundary", "http://evilgateway.example/mcp", []string{"gateway.example"}},
		{"a sanctioned host is not a sanctioned substring", "http://gateway.example.evil.test/mcp", []string{"gateway.example"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := composeWithConnector(t, gatewayDoc(tc.endpoint), ComposeOptions{
				Caller:               CallerIdentity{BearerToken: callerToken},
				ExpectedProject:      "demo-project",
				IdentityForwardHosts: tc.hosts,
			})
			if headers, ok := seen[tc.endpoint]; !ok {
				t.Fatal("endpoint should still be connected, just without identity")
			} else if len(headers) != 0 {
				t.Fatalf("unsanctioned endpoint received headers: %v", headers)
			}
		})
	}
}

// A sanctioned host still gets nothing when there is no identity to forward,
// or no authorized project to name.
func TestIdentity_NotForwardedWithoutTokenOrProject(t *testing.T) {
	const endpoint = "http://gateway.example/mcp"
	for _, tc := range []struct {
		name string
		opts ComposeOptions
	}{
		{"no caller token", ComposeOptions{ExpectedProject: "demo-project"}},
		{"no authorized project", ComposeOptions{Caller: CallerIdentity{BearerToken: callerToken}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.IdentityForwardHosts = []string{"gateway.example"}
			seen := composeWithConnector(t, gatewayDoc(endpoint), opts)
			if len(seen[endpoint]) != 0 {
				t.Fatalf("headers sent without a full identity: %v", seen[endpoint])
			}
		})
	}
}

// A sanctioned subdomain of a sanctioned domain is permitted, matching the
// SSRF allow-list's own suffix rule.
func TestIdentity_DomainSuffixMatch(t *testing.T) {
	const endpoint = "http://mcp.gateway.example/mcp"
	seen := composeWithConnector(t, gatewayDoc(endpoint), ComposeOptions{
		Caller:               CallerIdentity{BearerToken: callerToken},
		ExpectedProject:      "demo-project",
		IdentityForwardHosts: []string{"gateway.example"},
	})
	if seen[endpoint][AuthorizationHeader] == "" {
		t.Fatal("a subdomain of a sanctioned domain should receive identity")
	}
}

// Knowledge and skill bodies are static provider documents fetched by plain
// GET: they must never carry the caller's credential, even when the same
// document's MCP endpoint is sanctioned to receive it.
func TestIdentity_NotSentOnKnowledgeOrSkillFetches(t *testing.T) {
	var authSeen []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		authSeen = append(authSeen, req.Header.Get(AuthorizationHeader)+"|"+req.Header.Get(ProjectHeader))
		return fakeRoundTripper{pages: map[string]string{
			"http://provider/llms-full.txt": "StreamCo streams video at the edge.",
			"http://provider/skill.md":      "the procedure body",
		}}.RoundTrip(req)
	})}

	doc := streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.Tools.MCPServers[0].Endpoint = "http://gateway.example/mcp"
		d.Spec.Skills = []Skill{{Name: "triage", Description: "triage lag", Source: "http://provider/skill.md"}}
	})
	seen := map[string]map[string]string{}
	composed, err := Compose(context.Background(), []CapabilityDocument{doc}, ComposeOptions{
		HTTPClient:           client,
		Caller:               CallerIdentity{BearerToken: callerToken},
		ExpectedProject:      "demo-project",
		IdentityForwardHosts: []string{"gateway.example"},
		connect:              recordingConnector(seen, newFakeSession("streams_list", "pipeline_diagnose")),
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	defer composed.Close()

	// Load the skill body too, so the on-demand fetch is exercised.
	if _, err := composed.Tools[LoadSkillToolName].Execute(context.Background(),
		[]byte(`{"skill":"streamco__triage"}`)); err != nil {
		t.Fatalf("load_skill: %v", err)
	}

	if len(authSeen) == 0 {
		t.Fatal("no knowledge/skill fetch was made")
	}
	for _, got := range authSeen {
		if got != "|" {
			t.Fatalf("knowledge/skill fetch carried identity headers: %q", got)
		}
	}
	// …while the sanctioned MCP endpoint still got them.
	if seen["http://gateway.example/mcp"][AuthorizationHeader] == "" {
		t.Fatal("MCP endpoint should still receive identity")
	}
}

// The token must not be reachable through a stray format verb or log attr.
func TestCallerIdentity_RedactsTheToken(t *testing.T) {
	caller := CallerIdentity{BearerToken: callerToken}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("test", "caller", caller, "opts",
		ComposeOptions{Caller: caller, ExpectedProject: "demo-project"})
	if strings.Contains(buf.String(), callerToken) {
		t.Fatalf("token leaked into a log line: %s", buf.String())
	}
	if strings.Contains(caller.String(), callerToken) {
		t.Fatalf("token leaked through String(): %s", caller.String())
	}
}

// A failing provider must not turn the credential into a log line or an error
// message the model (or the user) can read back.
func TestIdentity_NotLeakedByComposeWarningsOrErrors(t *testing.T) {
	var buf bytes.Buffer
	doc := gatewayDoc("http://gateway.example/mcp")
	composed, err := Compose(context.Background(), []CapabilityDocument{doc}, ComposeOptions{
		Caller:               CallerIdentity{BearerToken: callerToken},
		ExpectedProject:      "demo-project",
		IdentityForwardHosts: []string{"gateway.example"},
		Logger:               testLogger(&buf),
		connect: func(context.Context, string, map[string]string) (mcpSession, error) {
			return nil, errFakeUnreachable
		},
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	defer composed.Close()

	if strings.Contains(buf.String(), callerToken) {
		t.Fatalf("token leaked into composition warnings: %s", buf.String())
	}
}

// Case, surrounding whitespace, a trailing dot, and blank entries must not
// change who is sanctioned — an operator's list is read the way they wrote it.
func TestIdentity_ForwardHostsAreNormalized(t *testing.T) {
	const endpoint = "http://gateway.example/mcp"
	seen := composeWithConnector(t, gatewayDoc(endpoint), ComposeOptions{
		Caller:               CallerIdentity{BearerToken: callerToken},
		ExpectedProject:      "demo-project",
		IdentityForwardHosts: []string{"  ", "GATEWAY.Example."},
	})
	if seen[endpoint][AuthorizationHeader] == "" {
		t.Fatal("case/whitespace/trailing-dot differences must not defeat the sanctioned list")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
