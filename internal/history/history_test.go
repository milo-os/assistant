package history

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestMemoryStoreCapsStoredContentLength(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	huge := strings.Repeat("x", MaxStoredContentLen+4096)
	if err := s.Append(ctx, "p", "c", Turn{UserText: huge, AssistantText: huge}); err != nil {
		t.Fatal(err)
	}
	turns, err := s.Turns(ctx, "p", "c")
	if err != nil || len(turns) != 1 {
		t.Fatalf("got %d turns, %v; want 1", len(turns), err)
	}
	if len(turns[0].UserText) != MaxStoredContentLen || len(turns[0].AssistantText) != MaxStoredContentLen {
		t.Fatalf("content not capped: user=%d assistant=%d, want %d",
			len(turns[0].UserText), len(turns[0].AssistantText), MaxStoredContentLen)
	}
}

func TestMemoryStoreEvictsOldestBeyondCap(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	const extra = 5
	for i := 0; i < MaxTurnsPerConversation+extra; i++ {
		if err := s.Append(ctx, "p", "c", Turn{
			UserText:      fmt.Sprintf("u%d", i),
			AssistantText: fmt.Sprintf("a%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := s.Turns(ctx, "p", "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != MaxTurnsPerConversation {
		t.Fatalf("got %d turns, want cap of %d", len(turns), MaxTurnsPerConversation)
	}
	// The oldest `extra` turns must have been dropped: the retained window
	// starts at turn `extra` and ends at the newest.
	if turns[0].UserText != fmt.Sprintf("u%d", extra) {
		t.Fatalf("oldest retained turn = %q, want %q", turns[0].UserText, fmt.Sprintf("u%d", extra))
	}
	want := fmt.Sprintf("u%d", MaxTurnsPerConversation+extra-1)
	if turns[len(turns)-1].UserText != want {
		t.Fatalf("newest retained turn = %q, want %q", turns[len(turns)-1].UserText, want)
	}
}

func TestTruncateContentBacksOffToRuneBoundary(t *testing.T) {
	// A multi-byte rune straddling the cut must not be split — the result stays
	// valid UTF-8 (a Postgres text column would reject a torn rune).
	s := strings.Repeat("a", MaxStoredContentLen-1) + "é" // 'é' is 2 bytes
	got := truncateContent(s)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateContent produced invalid UTF-8 of len %d", len(got))
	}
	if len(got) != MaxStoredContentLen-1 {
		t.Fatalf("got len %d, want %d (dropped the straddling rune)", len(got), MaxStoredContentLen-1)
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
