package history

import (
	"context"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore"
)

func TestMemoryStoreRoundTrip(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	turns, err := s.Turns(ctx, "proj", "ctx-1")
	if err != nil || turns != nil {
		t.Fatalf("empty store: got %v, %v; want nil, nil", turns, err)
	}

	if err := s.Append(ctx, "proj", "ctx-1", Turn{UserText: "u1", AssistantText: "a1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, "proj", "ctx-1", Turn{UserText: "u2", AssistantText: "a2"}); err != nil {
		t.Fatal(err)
	}

	turns, err = s.Turns(ctx, "proj", "ctx-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].UserText != "u1" || turns[1].AssistantText != "a2" {
		t.Fatalf("unexpected turns: %+v", turns)
	}
}

func TestMemoryStoreIsolatesByProjectAndContext(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_ = s.Append(ctx, "proj-a", "ctx-1", Turn{UserText: "secret", AssistantText: "noted"})

	for _, tc := range []struct{ project, context string }{
		{"proj-b", "ctx-1"}, // same context id, other project — the authz boundary
		{"proj-a", "ctx-2"},
	} {
		turns, err := s.Turns(ctx, tc.project, tc.context)
		if err != nil || turns != nil {
			t.Errorf("Turns(%q,%q) = %v, %v; want nil, nil", tc.project, tc.context, turns, err)
		}
	}
}

func TestMemoryStoreTurnsReturnsCopy(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_ = s.Append(ctx, "p", "c", Turn{UserText: "u", AssistantText: "a"})
	turns, _ := s.Turns(ctx, "p", "c")
	turns[0].UserText = "mutated"
	again, _ := s.Turns(ctx, "p", "c")
	if again[0].UserText != "u" {
		t.Fatal("Turns exposed internal state to caller mutation")
	}
}

func TestTruncateDropsOldestWholeTurns(t *testing.T) {
	// Each turn is ~100 chars => ~25 estimated tokens.
	big := strings.Repeat("x", 50)
	turns := []Turn{
		{UserText: big, AssistantText: big}, // oldest
		{UserText: big, AssistantText: big},
		{UserText: big, AssistantText: big}, // newest
	}

	if got := Truncate(turns, 1000); len(got) != 3 {
		t.Fatalf("large budget: kept %d turns, want 3", len(got))
	}
	if got := Truncate(turns, 55); len(got) != 2 || &got[0] == &turns[0] {
		t.Fatalf("budget for two turns: kept %d, want the 2 newest", len(got))
	}
	if got := Truncate(turns, 25); len(got) != 1 {
		t.Fatalf("budget for one turn: kept %d, want 1", len(got))
	}
	if got := Truncate(turns, 0); got != nil {
		t.Fatalf("zero budget: got %v, want nil", got)
	}
	if got := Truncate(nil, 100); got != nil {
		t.Fatalf("nil turns: got %v, want nil", got)
	}
}

func TestTruncateKeepsNewest(t *testing.T) {
	turns := []Turn{
		{UserText: strings.Repeat("a", 400)},
		{UserText: "newest"},
	}
	got := Truncate(turns, 50)
	if len(got) != 1 || got[0].UserText != "newest" {
		t.Fatalf("got %+v, want just the newest turn", got)
	}
}

func TestMessagesAlternatesRoles(t *testing.T) {
	msgs := Messages([]Turn{{UserText: "hi", AssistantText: "hello"}})
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != agentcore.RoleUser || msgs[0].Content[0].Text != "hi" {
		t.Fatalf("bad user message: %+v", msgs[0])
	}
	if msgs[1].Role != agentcore.RoleAssistant || msgs[1].Content[0].Text != "hello" {
		t.Fatalf("bad assistant message: %+v", msgs[1])
	}
	if Messages(nil) != nil {
		t.Fatal("Messages(nil) should be nil")
	}
}
