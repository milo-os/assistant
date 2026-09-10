package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"testing"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
	"github.com/milo-os/assistant/internal/agent"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/config"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
)

func loadTestConfig(t *testing.T, extra map[string]string) *config.Config {
	t.Helper()
	env := map[string]string{
		"AUTHN_TOKENREVIEW_API_URL": "https://control-plane.test",
		"AUTHZ_SAR_API_URL":         "https://control-plane.test",
	}
	maps.Copy(env, extra)
	cfg, err := config.Load(config.MapGetenv(env))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestNewAgentRunner_PersonaPromptFileMissing(t *testing.T) {
	cfg := loadTestConfig(t, map[string]string{"PERSONA_PROMPT_FILE": filepath.Join(t.TempDir(), "missing.md")})
	_, _, _, err := newAgentRunner(context.Background(), cfg, slog.New(slog.DiscardHandler), appmetrics.New())
	if err == nil {
		t.Fatal("want error for missing persona prompt file, got nil")
	}
}

func TestNewAgentRunner_PersonaPromptFileRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persona.md")
	if err := os.WriteFile(path, []byte("You are Acme's helper.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadTestConfig(t, map[string]string{"PERSONA_PROMPT_FILE": path})
	_, _, cleanup, err := newAgentRunner(context.Background(), cfg, slog.New(slog.DiscardHandler), appmetrics.New())
	if err != nil {
		t.Fatalf("newAgentRunner: %v", err)
	}
	cleanup()
}

// recordingSink captures the tool lifecycle callbacks the runner makes.
type recordingSink struct {
	text    []string
	started []assistanta2a.ToolActivity
	done    []assistanta2a.ToolActivity
}

func (s *recordingSink) OnTextDelta(t string)                     { s.text = append(s.text, t) }
func (s *recordingSink) OnToolStart(a assistanta2a.ToolActivity)  { s.started = append(s.started, a) }
func (s *recordingSink) OnToolFinish(a assistanta2a.ToolActivity) { s.done = append(s.done, a) }

func TestToolActivityTracker_PairsCallsWithResults(t *testing.T) {
	sink := &recordingSink{}
	tr := newToolActivityTracker()

	tr.forward(agent.Event{
		Kind: agent.EventToolCall, ToolName: "list_workloads", ToolCallID: "call-1",
		ToolInput: json.RawMessage(`{"project":"demo","token":"sk-secret"}`),
	}, sink)
	tr.forward(agent.Event{
		Kind: agent.EventToolResult, ToolName: "list_workloads", ToolCallID: "call-1",
	}, sink)

	if len(sink.started) != 1 || sink.started[0].Name != "list_workloads" {
		t.Fatalf("started = %+v", sink.started)
	}
	if sink.started[0].Summary != "project=demo, token=[redacted]" {
		t.Errorf("summary = %q, want the redacted argument line", sink.started[0].Summary)
	}
	if len(sink.done) != 1 || !sink.done[0].OK {
		t.Fatalf("finished = %+v, want one successful call", sink.done)
	}
	if sink.done[0].Elapsed <= 0 {
		t.Errorf("elapsed = %s, want the measured gap between call and result", sink.done[0].Elapsed)
	}
}

func TestToolActivityTracker_FailedResultAndMissingID(t *testing.T) {
	sink := &recordingSink{}
	tr := newToolActivityTracker()

	// No provider tool-call id: the tool's name is what pairs the two halves.
	tr.forward(agent.Event{Kind: agent.EventToolCall, ToolName: "load_skill"}, sink)
	tr.forward(agent.Event{Kind: agent.EventToolResult, ToolName: "load_skill", ToolFailed: true}, sink)

	if len(sink.done) != 1 {
		t.Fatalf("finished = %+v, want one call", sink.done)
	}
	if sink.done[0].OK {
		t.Error("a failed tool result must report OK=false")
	}
	if sink.done[0].Name != "load_skill" {
		t.Errorf("name = %q", sink.done[0].Name)
	}
}

// A result whose event carries no name still names the tool, from the call.
func TestToolActivityTracker_RemembersNameForResult(t *testing.T) {
	sink := &recordingSink{}
	tr := newToolActivityTracker()
	tr.forward(agent.Event{Kind: agent.EventToolCall, ToolName: "diagnose", ToolCallID: "c1"}, sink)
	tr.forward(agent.Event{Kind: agent.EventToolResult, ToolCallID: "c1"}, sink)
	if len(sink.done) != 1 || sink.done[0].Name != "diagnose" {
		t.Fatalf("finished = %+v, want the name carried over from the call", sink.done)
	}
}

// ── Capability source wiring ──────────────────────────────────

const platformFixtureJSON = `[{
  "metadata": { "name": "locations-platform-binding", "namespace": "milo-system" },
  "spec": {
    "serviceRef": { "name": "locations" },
    "serviceName": "locations.miloapis.com",
    "tools": {
      "mcpServers": [{
        "name": "locations",
        "endpoint": "http://locations-mcp.locations-system.svc.cluster.local:8080/mcp",
        "toolSelector": { "include": ["locations_list"] }
      }]
    }
  }
}]`

func writeJSON(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// With no platform source configured, nothing changes: the project source is
// handed to the agent exactly as before.
func TestNewCapabilitySource_ProjectOnly(t *testing.T) {
	cfg := loadTestConfig(t, map[string]string{"CAPABILITY_DOCS_FIXTURE": "/tmp/caps.json"})
	source := newCapabilitySource(cfg, slog.New(slog.DiscardHandler))
	if _, ok := source.(*capability.FixtureSource); !ok {
		t.Fatalf("source = %T, want *capability.FixtureSource", source)
	}
}

func TestNewCapabilitySource_NothingConfigured(t *testing.T) {
	if source := newCapabilitySource(loadTestConfig(t, nil), slog.New(slog.DiscardHandler)); source != nil {
		t.Fatalf("source = %T, want nil", source)
	}
}

// The wiring that matters: a platform fixture reaches a project that nothing
// entitled, carrying no namespace and marked platform (so composition leaves
// it off the meter).
func TestNewCapabilitySource_PlatformDocumentsReachEveryProject(t *testing.T) {
	cfg := loadTestConfig(t, map[string]string{
		"CAPABILITY_DOCS_FIXTURE":          writeJSON(t, "caps.json", "[]"),
		"PLATFORM_CAPABILITY_DOCS_FIXTURE": writeJSON(t, "platform-caps.json", platformFixtureJSON),
	})
	source := newCapabilitySource(cfg, slog.New(slog.DiscardHandler))
	if _, ok := source.(*capability.PlatformSource); !ok {
		t.Fatalf("source = %T, want *capability.PlatformSource", source)
	}

	docs, err := source.Documents(context.Background(), "some-unrelated-project")
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("want the platform document, got %d", len(docs))
	}
	if !docs[0].IsPlatform() {
		t.Error("the platform document must be marked platform")
	}
	if docs[0].Metadata.Namespace != "" {
		t.Errorf("namespace = %q, want cleared so every project keeps it", docs[0].Metadata.Namespace)
	}
	if kept := capability.ScopeDocuments(docs, "some-unrelated-project", slog.New(slog.DiscardHandler)); len(kept) != 1 {
		t.Error("the project scope gate dropped a platform document")
	}
}

// A platform source with no project source is a valid deployment: every
// project gets the platform's capabilities and nothing else.
func TestNewCapabilitySource_PlatformOnly(t *testing.T) {
	cfg := loadTestConfig(t, map[string]string{
		"PLATFORM_CAPABILITY_DOCS_FIXTURE": writeJSON(t, "platform-caps.json", platformFixtureJSON),
	})
	docs, err := newCapabilitySource(cfg, slog.New(slog.DiscardHandler)).Documents(context.Background(), "demo-project")
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs = %d, err = %v", len(docs), err)
	}
}

// The unmetered list is config, not a compile-time constant, and it has to
// reach the composition that reads it.
func TestNewAgentRunner_ForwardsUnmeteredServices(t *testing.T) {
	cfg := loadTestConfig(t, map[string]string{"CAPABILITY_UNMETERED_SERVICES": "quota.miloapis.com"})
	if len(cfg.CapabilityUnmeteredServices) != 1 {
		t.Fatalf("config did not parse the list: %v", cfg.CapabilityUnmeteredServices)
	}
	if _, _, cleanup, err := newAgentRunner(context.Background(), cfg, slog.New(slog.DiscardHandler), appmetrics.New()); err != nil {
		t.Fatalf("newAgentRunner: %v", err)
	} else {
		cleanup()
	}
}
