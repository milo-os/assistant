package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/memory"
)

// Project memory: durable, project-scoped facts (goals, conventions,
// decisions, standing constraints) that persist across conversations and are
// visible to every user working on the same project — distinct from
// conversation history, which is per-(project, contextId) and windowed. See
// internal/memory for the storage layer.
const (
	// RememberMemoryToolName is the model-facing name of the fact-writing tool.
	RememberMemoryToolName = "memory_remember"
	// ForgetMemoryToolName is the model-facing name of the fact-deleting tool.
	ForgetMemoryToolName = "memory_forget"
)

// buildMemoryIndex renders the project's current facts into a system-prompt
// section, or "" when there are none — mirroring buildSkillsIndex's
// "nothing to show, contribute nothing" shape.
func buildMemoryIndex(facts []memory.Fact) string {
	if len(facts) == 0 {
		return ""
	}
	sorted := make([]memory.Fact, len(facts))
	copy(sorted, facts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	var b strings.Builder
	b.WriteString("Project memory (persists across conversations and users — treat as established fact unless the current request contradicts it):\n")
	for _, f := range sorted {
		fmt.Fprintf(&b, "- %s: %s\n", f.Key, f.Value)
	}
	return strings.TrimRight(b.String(), "\n")
}

// rememberMemoryTool is the built-in [agentcore.Tool] that writes or updates
// a project fact. It fires no tool-invocation metering, matching
// loadSkillTool — this is platform memory, not a billable provider call.
type rememberMemoryTool struct {
	store   memory.Store
	project string
}

func (t *rememberMemoryTool) Definition() agentcore.ToolDefinition {
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "A short, stable identifier for this fact (e.g. \"deploy-target\", \"coding-style\").",
			},
			"value": map[string]any{
				"type":        "string",
				"description": "The fact to remember.",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Set true only after the user has confirmed overwriting a conflicting existing fact reported by a prior call.",
			},
		},
		"required": []string{"key", "value"},
	})
	return agentcore.ToolDefinition{
		Name: RememberMemoryToolName,
		Description: "Store or update a durable fact about this project (goals, conventions, decisions, standing " +
			"constraints) that persists across conversations and is visible to every user working on this project. " +
			"Use it only for facts worth remembering long-term, not one-off details. If a conflicting fact already " +
			"exists under the same key, this returns the existing value instead of overwriting it — ask the user to " +
			"confirm, then call again with confirm=true to overwrite.",
		InputSchema: schema,
	}
}

func (t *rememberMemoryTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Key     string `json:"key"`
		Value   string `json:"value"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.Unmarshal(input, &args); err != nil || args.Key == "" || args.Value == "" {
		return "", fmt.Errorf("memory_remember: input must be {\"key\": \"...\", \"value\": \"...\"}")
	}

	existing, ok, err := t.store.Get(ctx, t.project, args.Key)
	if err != nil {
		return "", fmt.Errorf("memory_remember: project memory is temporarily unavailable")
	}
	if ok && existing.Value != args.Value && !args.Confirm {
		return fmt.Sprintf(
			"A project memory fact for %q already exists: %q. Ask the user to confirm before overwriting it, "+
				"then call memory_remember again with confirm=true if they agree — or use a different key.",
			args.Key, existing.Value), nil
	}

	if err := t.store.Upsert(ctx, t.project, args.Key, args.Value); err != nil {
		if errors.Is(err, memory.ErrValueTooLong) {
			return "", fmt.Errorf("memory_remember: that value is too long to remember — try a shorter summary")
		}
		if errors.Is(err, memory.ErrProjectFull) {
			return "", fmt.Errorf("memory_remember: project memory is full — forget something first")
		}
		return "", fmt.Errorf("memory_remember: project memory is temporarily unavailable")
	}
	return fmt.Sprintf("Remembered: %s = %s", args.Key, args.Value), nil
}

// forgetMemoryTool is the built-in [agentcore.Tool] that deletes a project
// fact.
type forgetMemoryTool struct {
	store   memory.Store
	project string
}

func (t *forgetMemoryTool) Definition() agentcore.ToolDefinition {
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "The key of the fact to forget, as previously passed to memory_remember.",
			},
		},
		"required": []string{"key"},
	})
	return agentcore.ToolDefinition{
		Name:        ForgetMemoryToolName,
		Description: "Remove a previously remembered project fact by key.",
		InputSchema: schema,
	}
}

func (t *forgetMemoryTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(input, &args); err != nil || args.Key == "" {
		return "", fmt.Errorf("memory_forget: input must be {\"key\": \"...\"}")
	}
	_, ok, err := t.store.Get(ctx, t.project, args.Key)
	if err != nil {
		return "", fmt.Errorf("memory_forget: project memory is temporarily unavailable")
	}
	if !ok {
		return fmt.Sprintf("No project memory found for %q.", args.Key), nil
	}
	if err := t.store.Delete(ctx, t.project, args.Key); err != nil {
		return "", fmt.Errorf("memory_forget: project memory is temporarily unavailable")
	}
	return fmt.Sprintf("Forgot %q.", args.Key), nil
}
