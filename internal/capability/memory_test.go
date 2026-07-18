package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/memory"
)

func TestMemoryComposeIndexAndTools(t *testing.T) {
	store := memory.NewMemoryStore()
	if err := store.Upsert(context.Background(), "demo-project", "deploy-target", "kind cluster, namespace patch-playground"); err != nil {
		t.Fatal(err)
	}

	composed, err := Compose(context.Background(), nil, ComposeOptions{
		Memory:          store,
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	if !strings.Contains(composed.SystemPromptAddendum, "Project memory (persists across conversations and users") {
		t.Fatalf("addendum missing memory section:\n%s", composed.SystemPromptAddendum)
	}
	if !strings.Contains(composed.SystemPromptAddendum, "- deploy-target: kind cluster, namespace patch-playground") {
		t.Fatalf("addendum missing fact:\n%s", composed.SystemPromptAddendum)
	}
	if _, ok := composed.Tools[RememberMemoryToolName]; !ok {
		t.Fatal("memory_remember not composed")
	}
	if _, ok := composed.Tools[ForgetMemoryToolName]; !ok {
		t.Fatal("memory_forget not composed")
	}
}

func TestMemoryNoStoreOrNoProjectMeansNoTools(t *testing.T) {
	// Memory set but no ExpectedProject: tools need a project to scope to.
	composed, err := Compose(context.Background(), nil, ComposeOptions{Memory: memory.NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()
	if _, ok := composed.Tools[RememberMemoryToolName]; ok {
		t.Fatal("memory_remember composed with no ExpectedProject")
	}
	if composed.SystemPromptAddendum != "" {
		t.Fatalf("unexpected addendum: %q", composed.SystemPromptAddendum)
	}

	// ExpectedProject set but no Memory store: feature is simply off.
	composed2, err := Compose(context.Background(), nil, ComposeOptions{ExpectedProject: "demo-project"})
	if err != nil {
		t.Fatal(err)
	}
	defer composed2.Close()
	if _, ok := composed2.Tools[RememberMemoryToolName]; ok {
		t.Fatal("memory_remember composed with no Memory store")
	}
}

func TestMemoryNoFactsMeansNoAddendumSection(t *testing.T) {
	composed, err := Compose(context.Background(), nil, ComposeOptions{
		Memory:          memory.NewMemoryStore(),
		ExpectedProject: "demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()
	if composed.SystemPromptAddendum != "" {
		t.Fatalf("unexpected addendum with zero facts: %q", composed.SystemPromptAddendum)
	}
	// The tools still register even with nothing yet to show — the model can
	// still remember its first fact.
	if _, ok := composed.Tools[RememberMemoryToolName]; !ok {
		t.Fatal("memory_remember should compose even with zero current facts")
	}
}

func TestMemoryRememberConflictFlow(t *testing.T) {
	store := memory.NewMemoryStore()
	tool := &rememberMemoryTool{store: store, project: "demo-project"}
	ctx := context.Background()

	// First write: no existing fact, just writes.
	out, err := tool.Execute(ctx, json.RawMessage(`{"key":"style","value":"terse"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Remembered: style = terse") {
		t.Fatalf("unexpected result: %q", out)
	}

	// Same value again: no conflict, just writes (idempotent).
	out, err = tool.Execute(ctx, json.RawMessage(`{"key":"style","value":"terse"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Remembered") {
		t.Fatalf("same-value upsert should just write: %q", out)
	}

	// Different value, no confirm: must NOT overwrite, must report the conflict.
	out, err = tool.Execute(ctx, json.RawMessage(`{"key":"style","value":"verbose"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `already exists: "terse"`) {
		t.Fatalf("expected conflict report, got: %q", out)
	}
	f, ok, _ := store.Get(ctx, "demo-project", "style")
	if !ok || f.Value != "terse" {
		t.Fatalf("conflicting write without confirm must not overwrite; got %+v, %v", f, ok)
	}

	// Different value, confirm=true: overwrites.
	out, err = tool.Execute(ctx, json.RawMessage(`{"key":"style","value":"verbose","confirm":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Remembered: style = verbose") {
		t.Fatalf("confirmed overwrite should write: %q", out)
	}
	f, ok, _ = store.Get(ctx, "demo-project", "style")
	if !ok || f.Value != "verbose" {
		t.Fatalf("confirmed overwrite did not take: %+v, %v", f, ok)
	}
}

func TestMemoryRememberBounds(t *testing.T) {
	store := memory.NewMemoryStore()
	tool := &rememberMemoryTool{store: store, project: "demo-project"}
	ctx := context.Background()

	tooLong := strings.Repeat("x", memory.MaxFactValueLen+1)
	_, err := tool.Execute(ctx, json.RawMessage(`{"key":"k","value":"`+tooLong+`"}`))
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("want a too-long error, got %v", err)
	}

	for i := range memory.MaxFactsPerProject {
		if err := store.Upsert(ctx, "demo-project", fmt.Sprintf("k%d", i), "v"); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	_, err = tool.Execute(ctx, json.RawMessage(`{"key":"one-too-many","value":"v"}`))
	if err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("want a project-full error, got %v", err)
	}
}

func TestMemoryForgetPresentAndAbsent(t *testing.T) {
	store := memory.NewMemoryStore()
	ctx := context.Background()
	if err := store.Upsert(ctx, "demo-project", "k", "v"); err != nil {
		t.Fatal(err)
	}
	tool := &forgetMemoryTool{store: store, project: "demo-project"}

	out, err := tool.Execute(ctx, json.RawMessage(`{"key":"k"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Forgot "k"`) {
		t.Fatalf("unexpected result: %q", out)
	}
	if _, ok, _ := store.Get(ctx, "demo-project", "k"); ok {
		t.Fatal("fact should be gone")
	}

	// Forgetting an absent key is a benign no-op, not an error.
	out, err = tool.Execute(ctx, json.RawMessage(`{"key":"never-existed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No project memory found") {
		t.Fatalf("unexpected result: %q", out)
	}
}
