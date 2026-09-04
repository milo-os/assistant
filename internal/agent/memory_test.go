package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/memory"
)

func hasTool(tools []agentcore.ToolDefinition, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// TestMemoryNilDepsComposesNoTools pins the default: no Deps.Memory, no
// memory_remember/memory_forget tools, no addendum section — the feature is
// entirely opt-in.
func TestMemoryNilDepsComposesNoTools(t *testing.T) {
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter()})
	runTurn(t, conv, Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-1"})

	req := model.requests[0]
	if hasTool(req.Tools, capability.RememberMemoryToolName) {
		t.Fatal("memory_remember composed with Deps.Memory nil")
	}
	if containsMemorySection(req.System) {
		t.Fatalf("system prompt has a memory section with Deps.Memory nil:\n%s", req.System)
	}
}

// TestMemoryDepsSetComposesToolsAndSurfacesFacts proves the wiring end to
// end: with Deps.Memory set and a fact already stored for the project, the
// tools are composed and the current fact reaches the system prompt.
func TestMemoryDepsSetComposesToolsAndSurfacesFacts(t *testing.T) {
	store := memory.NewMemoryStore()
	if err := store.Upsert(context.Background(), "demo-project", "deploy-target", "kind cluster"); err != nil {
		t.Fatal(err)
	}
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(), Memory: store})
	runTurn(t, conv, Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-1"})

	req := model.requests[0]
	if !hasTool(req.Tools, capability.RememberMemoryToolName) {
		t.Fatal("memory_remember not composed with Deps.Memory set")
	}
	if !hasTool(req.Tools, capability.ForgetMemoryToolName) {
		t.Fatal("memory_forget not composed with Deps.Memory set")
	}
	if !containsMemorySection(req.System) {
		t.Fatalf("system prompt missing memory section:\n%s", req.System)
	}
}

// TestMemoryIsProjectScopedNotConversationScoped: a fact remembered while
// running one conversation is visible from a second, different conversation
// in the SAME project — proving memory persists across conversations and
// users, unlike history which is per-(project, contextId).
func TestMemoryIsProjectScopedNotConversationScoped(t *testing.T) {
	store := memory.NewMemoryStore()
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(), Memory: store})

	// Simulate a fact already learned (as if memory_remember had been called
	// during an earlier conversation in this project).
	if err := store.Upsert(context.Background(), "demo-project", "goal", "migrate to v2 API"); err != nil {
		t.Fatal(err)
	}

	runTurn(t, conv, Params{UserText: "hi", ProjectName: "demo-project", ContextID: "brand-new-conversation"})

	req := model.requests[0]
	if !containsMemorySection(req.System) {
		t.Fatalf("a new conversation in the same project should see prior project memory:\n%s", req.System)
	}
}

// TestMemoryDifferentProjectDoesNotSeeFacts: project is the isolation
// boundary for memory too, same as history.
func TestMemoryDifferentProjectDoesNotSeeFacts(t *testing.T) {
	store := memory.NewMemoryStore()
	if err := store.Upsert(context.Background(), "proj-a", "secret", "value"); err != nil {
		t.Fatal(err)
	}
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(), Memory: store})
	runTurn(t, conv, Params{UserText: "hi", ProjectName: "proj-b", ContextID: "conv-1"})

	req := model.requests[0]
	if containsMemorySection(req.System) {
		t.Fatalf("proj-b should not see proj-a's memory:\n%s", req.System)
	}
}

func containsMemorySection(system string) bool {
	return strings.Contains(system, "Project memory (persists across conversations and users")
}
