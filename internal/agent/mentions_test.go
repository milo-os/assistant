package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/usage"
)

func TestMentionNote(t *testing.T) {
	got := mentionNote("demo-project", []Mention{
		{Kind: "workload", Name: "api-backend", APIGroup: "compute.datumapis.com"},
		{Kind: "httpproxy", Name: "edge"},
	})
	want := "The user referenced these resources in demo-project: " +
		"workload/api-backend (compute.datumapis.com), httpproxy/edge. " +
		"Prefer looking them up directly."
	if got != want {
		t.Fatalf("note =\n%s\nwant\n%s", got, want)
	}
}

func TestMentionNoteEmptyWithoutMentions(t *testing.T) {
	if got := mentionNote("demo-project", nil); got != "" {
		t.Fatalf("note = %q, want empty", got)
	}
	// Half-formed entries carry no information and must not produce a note of
	// their own.
	if got := mentionNote("demo-project", []Mention{{Kind: "workload"}, {Name: "edge"}}); got != "" {
		t.Fatalf("note = %q, want empty", got)
	}
}

func TestMentionNoteDedupesAndCaps(t *testing.T) {
	ms := []Mention{{Kind: "workload", Name: "a"}, {Kind: "workload", Name: "a"}}
	for i := range maxMentionNoteEntries + 5 {
		ms = append(ms, Mention{Kind: "workload", Name: "w" + string(rune('a'+i%26))})
	}
	got := mentionNote("p", ms)
	if strings.Count(got, "workload/a,") != 1 {
		t.Errorf("duplicate mention not collapsed:\n%s", got)
	}
	if n := strings.Count(got, "workload/"); n != maxMentionNoteEntries {
		t.Errorf("note names %d resources, want the cap of %d", n, maxMentionNoteEntries)
	}
}

// The note has to land in the prompt the model actually sees, after the fixed
// operating rules rather than in place of them.
func TestMentionsReachTheModelSystemPrompt(t *testing.T) {
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: usage.NewEmitter(usage.EmitterConfig{})})

	stream := conv.Run(context.Background(), Params{
		UserText:    "why is @workload/api-backend down?",
		ProjectName: "demo-project",
		ContextID:   "conv-mentions",
		TaskID:      "t1",
		Mentions:    []Mention{{Kind: "workload", Name: "api-backend"}},
	})
	drainEvents(t, stream)

	if len(model.requests) == 0 {
		t.Fatal("model was never called")
	}
	system := model.requests[0].System
	if !strings.Contains(system, "workload/api-backend") {
		t.Fatalf("system prompt carries no mention note:\n%s", system)
	}
	if !strings.Contains(system, operatingRules) {
		t.Fatal("the mention note must be appended to the operating rules, not replace them")
	}
	if strings.Index(system, operatingRules) > strings.Index(system, "The user referenced") {
		t.Error("the note should come after the fixed prompt, not before it")
	}
}

// A turn with no mentions must produce byte-for-byte the prompt it did before
// this existed.
func TestNoMentionsLeavesTheSystemPromptUntouched(t *testing.T) {
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: usage.NewEmitter(usage.EmitterConfig{})})
	stream := conv.Run(context.Background(), Params{UserText: "hi", ProjectName: "demo-project", TaskID: "t1"})
	drainEvents(t, stream)

	if got := model.requests[0].System; got != BuildSystemPrompt("", "") {
		t.Fatalf("system prompt changed without mentions:\n%s", got)
	}
}
