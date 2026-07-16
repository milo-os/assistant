package capability

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func skillDoc(service, ref, name, desc, source string) CapabilityDocument {
	return CapabilityDocument{
		Spec: CapabilitySpec{
			ServiceRef:           Ref{Name: ref},
			ServiceName:          service,
			ServiceAgentRef:      Ref{Name: ref + "-agent"},
			ConfigurationVersion: "v1",
			Skills:               []Skill{{Name: name, Description: desc, Source: source}},
		},
	}
}

func TestSkillsComposeIndexAndTool(t *testing.T) {
	body := "## Lag triage\n1. Run pipeline_diagnose.\n2. Check CONSUMER_LAG first."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	composed, err := Compose(context.Background(), []CapabilityDocument{
		skillDoc("streaming.streamco.example", "streamco", "lag-triage", "Triage pipeline consumer lag", srv.URL+"/skills/lag.md"),
	}, ComposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	// Index: name + description in the prompt addendum; body ABSENT.
	if !strings.Contains(composed.SystemPromptAddendum, "streamco__lag-triage — Triage pipeline consumer lag") {
		t.Fatalf("addendum missing skill index entry:\n%s", composed.SystemPromptAddendum)
	}
	if strings.Contains(composed.SystemPromptAddendum, "Run pipeline_diagnose") {
		t.Fatal("skill BODY leaked into the prompt — progressive disclosure broken")
	}

	// The loader tool exists, advertises the skill in its enum, and fetches
	// the body with provenance framing on demand.
	tool, ok := composed.Tools[LoadSkillToolName]
	if !ok {
		t.Fatal("load_skill tool not composed")
	}
	if !strings.Contains(string(tool.Definition().InputSchema), "streamco__lag-triage") {
		t.Fatalf("tool schema missing skill enum: %s", tool.Definition().InputSchema)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"skill":"streamco__lag-triage"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Skill streamco__lag-triage (published by streaming.streamco.example") {
		t.Fatalf("missing provenance framing: %q", out)
	}
	if !strings.Contains(out, "Check CONSUMER_LAG first") {
		t.Fatalf("missing body: %q", out)
	}

	// Unknown skill errors clearly (the loop converts it to an error result).
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"skill":"nope"}`)); err == nil {
		t.Fatal("unknown skill should error")
	}
}

func TestSkillsNoSkillsMeansNoTool(t *testing.T) {
	composed, err := Compose(context.Background(), []CapabilityDocument{{
		Spec: CapabilitySpec{ServiceRef: Ref{Name: "x"}, ServiceName: "x.example",
			ServiceAgentRef: Ref{Name: "xa"}, ConfigurationVersion: "v1"},
	}}, ComposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()
	if _, ok := composed.Tools[LoadSkillToolName]; ok {
		t.Fatal("load_skill composed with no skills present")
	}
	if composed.SystemPromptAddendum != "" {
		t.Fatalf("unexpected addendum: %q", composed.SystemPromptAddendum)
	}
}

func TestSkillFetchFailureDegradesGracefully(t *testing.T) {
	composed, err := Compose(context.Background(), []CapabilityDocument{
		skillDoc("s.example", "s", "broken", "A skill with a dead source", "http://127.0.0.1:1/nope"),
	}, ComposeOptions{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()
	_, execErr := composed.Tools[LoadSkillToolName].Execute(context.Background(), json.RawMessage(`{"skill":"s__broken"}`))
	if execErr == nil {
		t.Fatal("dead source should error")
	}
	if !strings.Contains(execErr.Error(), "temporarily unavailable") {
		t.Fatalf("error should be user-safe, got: %v", execErr)
	}
}

func TestSkillValidation(t *testing.T) {
	doc := skillDoc("s.example", "s", "name", "desc", "http://example/skill.md")
	if err := doc.Validate(); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
	doc.Spec.Skills[0].Source = ""
	if err := doc.Validate(); err == nil || !strings.Contains(err.Error(), "spec.skills[0].source") {
		t.Fatalf("want source-required error, got %v", err)
	}
}
