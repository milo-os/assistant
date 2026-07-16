package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore/mockmodel"
	"github.com/milo-os/assistant/internal/capability"
)

// TestSkillLoadFullPath proves the whole skills path with no API key: the
// skill's description (not body) enters the prompt, the mock model calls the
// built-in load_skill tool, the body round-trips into the final answer, and
// NO tool-invocation billing event fires (loading instructions is not a
// provider tool call).
func TestSkillLoadFullPath(t *testing.T) {
	body := "1. Run streamco__pipeline_diagnose. 2. Read CONSUMER_LAG before anything else."
	skillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer skillSrv.Close()

	doc := capability.CapabilityDocument{
		Spec: capability.CapabilitySpec{
			ServiceRef:           capability.Ref{Name: "streamco"},
			ServiceName:          "streaming.streamco.example",
			ServiceAgentRef:      capability.Ref{Name: "streamco-agent"},
			ConfigurationVersion: "v1",
			Skills: []capability.Skill{{
				Name:        "lag-triage",
				Description: "Triage pipeline consumer lag step by step",
				Source:      skillSrv.URL + "/skills/lag.md",
			}},
		},
	}

	conv := New(Deps{
		Model:     mockmodel.New(),
		ModelMode: "mock",
		Source:    fakeSource{docs: []capability.CapabilityDocument{doc}},
		Emitter:   noopEmitter(),
	})

	stream := conv.Run(context.Background(), Params{
		UserText:    "Use the streamco__lag-triage skill",
		ProjectName: "demo-project", ContextID: "conv-skill", TaskID: "task-skill",
	})
	events := drainEvents(t, stream)
	result := stream.Result()

	if result.State != StateCompleted {
		t.Fatalf("state = %s (err=%s)", result.State, result.Error)
	}
	var sawLoadSkill bool
	for _, e := range events {
		if e.Kind == EventToolCall && e.ToolName == "load_skill" {
			sawLoadSkill = true
		}
	}
	if !sawLoadSkill {
		t.Fatal("expected a load_skill tool-call event")
	}
	if !strings.Contains(result.Text, "Following its procedure") ||
		!strings.Contains(result.Text, "Read CONSUMER_LAG before anything else") {
		t.Fatalf("final answer should quote the loaded skill body, got: %q", result.Text)
	}
	// Not a provider tool: no tool-invocation billing event.
	if result.Usage.ToolInvocationEventCount != 0 {
		t.Fatalf("load_skill must not meter as a provider tool invocation, got %d events",
			result.Usage.ToolInvocationEventCount)
	}
}
