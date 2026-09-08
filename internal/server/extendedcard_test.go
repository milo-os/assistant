package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/logger"
)

const (
	methodExtendedCard = "GetExtendedAgentCard"
	betaProject        = "beta-project"
)

// cardFixture is two projects' capability documents in one file, the shape a
// FixtureSource really returns: it ignores projectName and hands back
// everything, so [capability.ScopeDocuments] is the only project gate.
const cardFixture = `[
  {
    "apiVersion": "services.miloapis.com/v1alpha1",
    "kind": "AgentBinding",
    "metadata": { "name": "streamco-binding", "namespace": "demo-project" },
    "spec": {
      "serviceRef": { "name": "streamco" },
      "serviceName": "streaming.streamco.example",
      "serviceAgentRef": { "name": "streamco-agent" },
      "configurationVersion": "v1",
      "tools": {
        "mcpServers": [
          { "name": "streamco", "endpoint": "http://127.0.0.1:7810/mcp", "toolSelector": { "include": ["streams_list", "pipeline_diagnose"] } }
        ]
      },
      "skills": [
        { "name": "lag-triage", "description": "Triage pipeline consumer lag", "source": "http://127.0.0.1:7810/runbooks/lag.md" }
      ]
    }
  },
  {
    "apiVersion": "services.miloapis.com/v1alpha1",
    "kind": "AgentBinding",
    "metadata": { "name": "billco-binding", "namespace": "beta-project" },
    "spec": {
      "serviceRef": { "name": "billco" },
      "serviceName": "billing.billco.example",
      "serviceAgentRef": { "name": "billco-agent" },
      "configurationVersion": "v1",
      "tools": {
        "mcpServers": [
          { "name": "billco", "endpoint": "http://127.0.0.1:7820/mcp", "toolSelector": { "include": ["invoice_get"] } }
        ]
      }
    }
  }
]`

// fixtureAdvertiser is the [assistanta2a.SkillAdvertiser] these tests run
// against: it derives skills from capability documents through the SAME scope
// gate a turn's Compose uses, so a mis-scoped card shows up here as another
// project's service rather than as a passing hand-written map.
type fixtureAdvertiser struct {
	source capability.Source
	err    error

	mu    sync.Mutex
	asked []string
}

func newFixtureAdvertiser(t *testing.T) *fixtureAdvertiser {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capability-documents.json")
	if err := os.WriteFile(path, []byte(cardFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fixtureAdvertiser{source: capability.NewFixtureSource(path, logger.Silent())}
}

func (f *fixtureAdvertiser) ProjectSkills(ctx context.Context, req assistanta2a.CardRequest) ([]a2a.AgentSkill, error) {
	f.mu.Lock()
	f.asked = append(f.asked, req.ProjectName)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	docs, err := f.source.Documents(ctx, req.ProjectName)
	if err != nil {
		return nil, err
	}
	scoped := capability.ScopeDocuments(docs, req.ProjectName, logger.Silent())
	return assistanta2a.ServiceSkills(capability.Entitlements(scoped)), nil
}

func (f *fixtureAdvertiser) projects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// recordingAuthorizer records the project each authorization decision was asked
// about, so a test can pin that the card is built from the very project the
// caller was cleared for (no confused deputy).
type recordingAuthorizer struct {
	inner auth.Authorizer

	mu       sync.Mutex
	projects []string
}

func (a *recordingAuthorizer) AuthorizeProject(ctx context.Context, p auth.Principal, projectName string) error {
	a.mu.Lock()
	a.projects = append(a.projects, projectName)
	a.mu.Unlock()
	return a.inner.AuthorizeProject(ctx, p, projectName)
}

func (a *recordingAuthorizer) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.projects)
}

// newCardServer boots the app with an injected advertiser and authorizer; a nil
// authorizer uses the shared test grants (alice ⇒ demo-project only).
func newCardServer(t *testing.T, advertiser assistanta2a.SkillAdvertiser, authz auth.Authorizer) *httptest.Server {
	t.Helper()
	authn, defaultAuthz := testAuth()
	if authz == nil {
		authz = defaultAuthz
	}
	app := New(Deps{
		Config:          testConfig(t),
		Logger:          logger.Silent(),
		Authenticator:   authn,
		Authorizer:      authz,
		Runner:          fakeRunner{},
		SkillAdvertiser: advertiser,
	})
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return srv
}

func extendedCard(t *testing.T, r rpcResponse) *a2a.AgentCard {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", r.Error)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(r.Result, &card); err != nil {
		t.Fatalf("decode extended card: %v", err)
	}
	return &card
}

func skillIDs(card *a2a.AgentCard) []string {
	ids := make([]string, 0, len(card.Skills))
	for _, s := range card.Skills {
		ids = append(ids, s.ID)
	}
	return ids
}

// ── GetExtendedAgentCard ──────────────────────────────────────

func TestExtendedAgentCard_Unauthenticated(t *testing.T) {
	srv := newCardServer(t, newFixtureAdvertiser(t), nil)
	res := rpc(t, srv, "", methodExtendedCard, map[string]any{"tenant": project}, "x")
	defer res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

func TestExtendedAgentCard_403ForUngrantedProject(t *testing.T) {
	adv := newFixtureAdvertiser(t)
	srv := newCardServer(t, adv, nil)
	res := rpc(t, srv, wrongToken, methodExtendedCard, map[string]any{"tenant": project}, "x")
	defer res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	if got := adv.projects(); len(got) != 0 {
		t.Errorf("advertiser consulted for a denied project: %v", got)
	}
}

func TestExtendedAgentCard_AuthorizedListsProjectServices(t *testing.T) {
	adv := newFixtureAdvertiser(t)
	authz := &recordingAuthorizer{inner: stubAuthorizer{"alice": {project}}}
	srv := newCardServer(t, adv, authz)

	card := extendedCard(t, decodeRPC(t, rpc(t, srv, goodToken, methodExtendedCard,
		map[string]any{"tenant": project}, "x")))

	// The generic skill stays first so a zero-entitlement project still
	// advertises something.
	if got := skillIDs(card); !slices.Equal(got, []string{"project-assistant", "streamco"}) {
		t.Fatalf("skill ids = %v", got)
	}
	svc := card.Skills[1]
	if svc.Name != "streaming.streamco.example" {
		t.Errorf("service skill name = %q", svc.Name)
	}
	for _, want := range []string{"streamco__streams_list", "streamco__pipeline_diagnose"} {
		if !slices.Contains(svc.Tags, want) {
			t.Errorf("tags %v missing %q", svc.Tags, want)
		}
	}
	if !strings.Contains(svc.Description, "http://127.0.0.1:7810/mcp") {
		t.Errorf("description = %q, want the MCP endpoint", svc.Description)
	}
	// The extended card is the public card plus skills.
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].URL != "http://assistant.test/a2a" {
		t.Errorf("interfaces = %+v", card.SupportedInterfaces)
	}
	if _, ok := card.SecuritySchemes["bearer"]; !ok {
		t.Errorf("bearer security scheme missing")
	}

	// CONFUSED-DEPUTY PIN: the project authorized is the project the card was
	// built from.
	if got := authz.seen(); !slices.Equal(got, []string{project}) {
		t.Errorf("authorized projects = %v, want [%s]", got, project)
	}
	if got := adv.projects(); !slices.Equal(got, []string{project}) {
		t.Errorf("advertiser asked about %v, want [%s]", got, project)
	}
}

// TestExtendedAgentCard_PerProjectSkills is the bug this feature fixes: one
// server, two projects, two different skill sets — not one hardcoded card.
func TestExtendedAgentCard_PerProjectSkills(t *testing.T) {
	srv := newCardServer(t, newFixtureAdvertiser(t), stubAuthorizer{"alice": {project, betaProject}})

	demo := extendedCard(t, decodeRPC(t, rpc(t, srv, goodToken, methodExtendedCard,
		map[string]any{"tenant": project}, "a")))
	beta := extendedCard(t, decodeRPC(t, rpc(t, srv, goodToken, methodExtendedCard,
		map[string]any{"tenant": betaProject}, "b")))

	if got := skillIDs(demo); !slices.Equal(got, []string{"project-assistant", "streamco"}) {
		t.Errorf("%s skills = %v", project, got)
	}
	if got := skillIDs(beta); !slices.Equal(got, []string{"project-assistant", "billco"}) {
		t.Errorf("%s skills = %v", betaProject, got)
	}
	// Neither card may name the other project's service.
	if raw, _ := json.Marshal(beta); strings.Contains(string(raw), "streamco") {
		t.Errorf("%s card leaked streamco: %s", betaProject, raw)
	}
	if raw, _ := json.Marshal(demo); strings.Contains(string(raw), "billco") {
		t.Errorf("%s card leaked billco: %s", project, raw)
	}
}

// TestExtendedAgentCard_EmptyTenant pins the "can't resolve the project ⇒ no
// disclosure" convention: no authorization decision is made and the generic
// public card comes back.
func TestExtendedAgentCard_EmptyTenant(t *testing.T) {
	adv := newFixtureAdvertiser(t)
	authz := &recordingAuthorizer{inner: stubAuthorizer{"alice": {project}}}
	srv := newCardServer(t, adv, authz)

	card := extendedCard(t, decodeRPC(t, rpc(t, srv, goodToken, methodExtendedCard, map[string]any{}, "x")))
	if got := skillIDs(card); !slices.Equal(got, []string{"project-assistant"}) {
		t.Errorf("skills = %v, want the generic card only", got)
	}
	if got := authz.seen(); len(got) != 0 {
		t.Errorf("authorizer called with %v, want no call", got)
	}
	if got := adv.projects(); len(got) != 0 {
		t.Errorf("advertiser called with %v, want no call", got)
	}
}

// TestExtendedAgentCard_NoAdvertiser: an AgentRunner that can't describe
// entitlement answers the A2A "not configured" error rather than panicking or
// booting differently.
func TestExtendedAgentCard_NoAdvertiser(t *testing.T) {
	srv := newTestServer(t)
	r := decodeRPC(t, rpc(t, srv, goodToken, methodExtendedCard, map[string]any{"tenant": project}, "x"))
	if r.Error == nil || r.Error.Code != -32007 {
		t.Fatalf("want -32007 extended-card-not-configured, got %+v (result %s)", r.Error, r.Result)
	}
}

// TestExtendedAgentCard_AdvertiserErrorDegrades: discovery degrades to the
// generic card the way the capability source degrades to no documents — never a
// 500.
func TestExtendedAgentCard_AdvertiserErrorDegrades(t *testing.T) {
	adv := newFixtureAdvertiser(t)
	adv.err = errors.New("capability source unavailable")
	srv := newCardServer(t, adv, nil)

	res := rpc(t, srv, goodToken, methodExtendedCard, map[string]any{"tenant": project}, "x")
	if res.StatusCode >= 500 {
		res.Body.Close()
		t.Fatalf("status = %d, want a card not a server error", res.StatusCode)
	}
	card := extendedCard(t, decodeRPC(t, res))
	if got := skillIDs(card); !slices.Equal(got, []string{"project-assistant"}) {
		t.Errorf("skills = %v, want the generic card only", got)
	}
}

// TestSendMessage_TenantIgnored pins that SendMessage authorization still reads
// message.metadata.projectName and ignores "tenant": a2a-go's client transport
// stamps a tenant onto every request, so honoring it here would let a client
// default pick the authorized project.
func TestSendMessage_TenantIgnored(t *testing.T) {
	srv := newCardServer(t, newFixtureAdvertiser(t), nil)

	t.Run("disagreeing tenant does not narrow", func(t *testing.T) {
		params := sendMessageParams(project, "hi")
		params["tenant"] = "other-project"
		res := rpc(t, srv, goodToken, "SendMessage", params, "1")
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("status = %d, want 200 (authorized on projectName)", res.StatusCode)
		}
	})

	t.Run("disagreeing tenant does not widen", func(t *testing.T) {
		params := sendMessageParams("other-project", "hi")
		params["tenant"] = project
		res := rpc(t, srv, goodToken, "SendMessage", params, "2")
		defer res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatalf("status = %d, want 403 (authorized on projectName)", res.StatusCode)
		}
	})
}
