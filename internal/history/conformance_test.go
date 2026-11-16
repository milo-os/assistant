package history

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// storeConformance runs the behavioral contract every Store+Lister must
// satisfy. MemoryStore always runs it; PostgresStore runs it against
// TEST_DATABASE_URL when set (skipped otherwise, so `go test ./...` needs no
// database). The Postgres e2e path additionally proves restart survival —
// that cannot be expressed here.
func storeConformance(t *testing.T, newStore func(t *testing.T) interface {
	Store
	Editor
}) {
	ctx := context.Background()

	// unique keys per subtest so a shared database can rerun without cleanup
	fresh := func(name string) (string, string) {
		return "proj-" + name + "-" + uniqueSuffix(t), "ctx-" + name + "-" + uniqueSuffix(t)
	}

	t.Run("round-trip preserves order", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("rt")
		for i := 1; i <= 3; i++ {
			err := s.Append(ctx, project, contextID, Turn{
				UserText:      fmt.Sprintf("u%d", i),
				AssistantText: fmt.Sprintf("a%d", i),
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 3 {
			t.Fatalf("got %d turns, want 3: %+v", len(turns), turns)
		}
		for i, turn := range turns {
			if turn.UserText != fmt.Sprintf("u%d", i+1) || turn.AssistantText != fmt.Sprintf("a%d", i+1) {
				t.Fatalf("turn %d out of order: %+v", i, turn)
			}
		}
	})

	t.Run("unknown conversation is empty", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("empty")
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil || turns != nil {
			t.Fatalf("got %v, %v; want nil, nil", turns, err)
		}
	})

	t.Run("project isolation", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("iso")
		if err := s.Append(ctx, project, contextID, Turn{UserText: "secret", AssistantText: "noted"}); err != nil {
			t.Fatal(err)
		}
		// Same contextId, different project: the authz boundary.
		turns, err := s.Turns(ctx, project+"-other", contextID)
		if err != nil || turns != nil {
			t.Fatalf("cross-project read got %v, %v; want nil, nil", turns, err)
		}
		// And the other project's listing must not show it either.
		convs, err := s.ListConversations(ctx, project+"-other", ListOptions{Limit: 10})
		if err != nil || len(convs) != 0 {
			t.Fatalf("cross-project list got %v, %v; want empty", convs, err)
		}
	})

	t.Run("list conversations newest-activity first", func(t *testing.T) {
		s := newStore(t)
		project, _ := fresh("list")
		for _, id := range []string{"c1", "c2"} {
			if err := s.Append(ctx, project, id, Turn{UserText: "u", AssistantText: "a"}); err != nil {
				t.Fatal(err)
			}
		}
		// Touch c1 again so it becomes the most recent.
		if err := s.Append(ctx, project, "c1", Turn{UserText: "u2", AssistantText: "a2"}); err != nil {
			t.Fatal(err)
		}
		convs, err := s.ListConversations(ctx, project, ListOptions{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(convs) != 2 || convs[0].ContextID != "c1" || convs[0].TurnCount != 2 || convs[1].TurnCount != 1 {
			t.Fatalf("unexpected listing: %+v", convs)
		}
		if convs[0].CreatedAt.IsZero() || convs[0].LastActiveAt.Before(convs[0].CreatedAt) {
			t.Fatalf("bad timestamps: %+v", convs[0])
		}
		limited, err := s.ListConversations(ctx, project, ListOptions{Limit: 1})
		if err != nil || len(limited) != 1 {
			t.Fatalf("limit not honored: %v, %v", limited, err)
		}
	})

	t.Run("title is the opening user message, surviving compaction", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("title")
		opening := "Why is the  api-backend\nworkload not available?"
		for _, u := range []string{opening, "and the edge-cache one?", "thanks"} {
			if err := s.Append(ctx, project, contextID, Turn{UserText: u, AssistantText: "a"}); err != nil {
				t.Fatal(err)
			}
		}
		want := "Why is the api-backend workload not available?"
		convs, err := s.ListConversations(ctx, project, ListOptions{Limit: 10})
		if err != nil || len(convs) != 1 || convs[0].Title != want {
			t.Fatalf("list title = %+v, %v; want %q", convs, err, want)
		}
		got, err := s.GetConversation(ctx, project, contextID)
		if err != nil || got.Title != want {
			t.Fatalf("get title = %q, %v; want %q", got.Title, err, want)
		}
		// After compaction the summary turn's synthetic marker sits at the
		// head of the transcript; the title must skip it and pick the oldest
		// real user message that survived.
		// The summary replaces turn 1; turns 2 and 3 stay.
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), 1, 0); err != nil {
			t.Fatal(err)
		}
		got, err = s.GetConversation(ctx, project, contextID)
		if err != nil || got.Title != "and the edge-cache one?" {
			t.Fatalf("post-compact title = %q, %v", got.Title, err)
		}
		// Only a summary left: no title rather than the marker text.
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), 3, 1); err != nil {
			t.Fatal(err)
		}
		got, err = s.GetConversation(ctx, project, contextID)
		if err != nil || got.Title != "" {
			t.Fatalf("summary-only title = %q, %v; want empty", got.Title, err)
		}
	})

	t.Run("rename sets a name without disturbing the title", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("rename")
		if err := s.Append(ctx, project, contextID, Turn{UserText: "why is p-1 down?", AssistantText: "a"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Rename(ctx, project, contextID, "  dfw   quota\nescalation "); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetConversation(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		// Normalized (collapsed, trimmed) — and the derived title is still
		// there underneath, which is the point of keeping them separate.
		if got.Name != "dfw quota escalation" {
			t.Fatalf("name = %q, want the normalized name", got.Name)
		}
		if got.Title != "why is p-1 down?" {
			t.Fatalf("title = %q, want it untouched by the rename", got.Title)
		}
		convs, err := s.ListConversations(ctx, project, ListOptions{Limit: 10})
		if err != nil || len(convs) != 1 || convs[0].Name != "dfw quota escalation" {
			t.Fatalf("list = %+v, %v; want the name on the listed row", convs, err)
		}

		// Renaming again replaces; an empty name clears back to unnamed.
		if err := s.Rename(ctx, project, contextID, "second thoughts"); err != nil {
			t.Fatal(err)
		}
		if got, err = s.GetConversation(ctx, project, contextID); err != nil || got.Name != "second thoughts" {
			t.Fatalf("re-rename = %q, %v", got.Name, err)
		}
		if err := s.Rename(ctx, project, contextID, "   "); err != nil {
			t.Fatal(err)
		}
		if got, err = s.GetConversation(ctx, project, contextID); err != nil || got.Name != "" {
			t.Fatalf("cleared name = %q, %v; want empty", got.Name, err)
		}
	})

	t.Run("rename caps the name at MaxNameLen runes", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("renamelong")
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u", AssistantText: "a"}); err != nil {
			t.Fatal(err)
		}
		// Multi-byte runes, so a byte-wise cut would both truncate wrong and
		// risk splitting a rune on the way into a text column.
		long := strings.Repeat("é", MaxNameLen+40)
		if err := s.Rename(ctx, project, contextID, long); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetConversation(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if n := utf8.RuneCountInString(got.Name); n != MaxNameLen {
			t.Fatalf("name is %d runes, want %d", n, MaxNameLen)
		}
	})

	t.Run("rename of an unknown conversation is not found", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("renamemissing")
		err := s.Rename(ctx, project, contextID, "nope")
		if !errors.Is(err, ErrConversationNotFound) {
			t.Fatalf("err = %v, want ErrConversationNotFound", err)
		}
		// And it must not have created one on the way to failing.
		if convs, lErr := s.ListConversations(ctx, project, ListOptions{Limit: 10}); lErr != nil || len(convs) != 0 {
			t.Fatalf("list = %+v, %v; want empty", convs, lErr)
		}
	})

	t.Run("archive hides from the default listing and shows in the archived one", func(t *testing.T) {
		s := newStore(t)
		project, _ := fresh("archive")
		for _, id := range []string{"keep", "file-away"} {
			if err := s.Append(ctx, project, id, Turn{UserText: "u", AssistantText: "a"}); err != nil {
				t.Fatal(err)
			}
		}
		before, err := s.GetConversation(ctx, project, "file-away")
		if err != nil {
			t.Fatal(err)
		}
		if !before.ArchivedAt.IsZero() {
			t.Fatalf("a fresh conversation is archived: %+v", before)
		}
		if err := s.SetArchived(ctx, project, "file-away", true); err != nil {
			t.Fatal(err)
		}

		active, err := s.ListConversations(ctx, project, ListOptions{Limit: 10})
		if err != nil || len(active) != 1 || active[0].ContextID != "keep" {
			t.Fatalf("default list = %+v, %v; want only the unarchived conversation", active, err)
		}
		archived, err := s.ListConversations(ctx, project, ListOptions{Limit: 10, Archived: true})
		if err != nil || len(archived) != 1 || archived[0].ContextID != "file-away" || archived[0].ArchivedAt.IsZero() {
			t.Fatalf("archived list = %+v, %v; want only the archived conversation, with its time", archived, err)
		}

		// Still gettable by id — archive hides, it does not remove — and
		// filing it away is not activity in it.
		got, err := s.GetConversation(ctx, project, "file-away")
		if err != nil || got.ArchivedAt.IsZero() {
			t.Fatalf("get archived = %+v, %v; want it found with ArchivedAt set", got, err)
		}
		if !got.LastActiveAt.Equal(before.LastActiveAt) {
			t.Fatalf("archive moved last-active from %v to %v", before.LastActiveAt, got.LastActiveAt)
		}

		// Archiving again keeps the original time.
		time.Sleep(5 * time.Millisecond)
		if err := s.SetArchived(ctx, project, "file-away", true); err != nil {
			t.Fatal(err)
		}
		again, err := s.GetConversation(ctx, project, "file-away")
		if err != nil || !again.ArchivedAt.Equal(got.ArchivedAt) {
			t.Fatalf("re-archive ArchivedAt = %v, %v; want the original %v", again.ArchivedAt, err, got.ArchivedAt)
		}

		// Unarchive brings it back into the default listing.
		if err := s.SetArchived(ctx, project, "file-away", false); err != nil {
			t.Fatal(err)
		}
		back, err := s.ListConversations(ctx, project, ListOptions{Limit: 10})
		if err != nil || len(back) != 2 {
			t.Fatalf("list after unarchive = %+v, %v; want both", back, err)
		}
		for _, c := range back {
			if !c.ArchivedAt.IsZero() {
				t.Fatalf("unarchived conversation still carries ArchivedAt: %+v", c)
			}
		}
		if none, err := s.ListConversations(ctx, project, ListOptions{Limit: 10, Archived: true}); err != nil || len(none) != 0 {
			t.Fatalf("archived list after unarchive = %+v, %v; want empty", none, err)
		}
	})

	t.Run("a new turn unarchives; rename and compact do not", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("unarchive-on-append")
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u1", AssistantText: "a1"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetArchived(ctx, project, contextID, true); err != nil {
			t.Fatal(err)
		}
		if err := s.Rename(ctx, project, contextID, "named while archived"); err != nil {
			t.Fatal(err)
		}
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), 1, 0); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetConversation(ctx, project, contextID); err != nil || got.ArchivedAt.IsZero() {
			t.Fatalf("after rename+compact = %+v, %v; want still archived", got, err)
		}
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u2", AssistantText: "a2"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetConversation(ctx, project, contextID)
		if err != nil || !got.ArchivedAt.IsZero() {
			t.Fatalf("after append = %+v, %v; want unarchived", got, err)
		}
		if convs, err := s.ListConversations(ctx, project, ListOptions{Limit: 10}); err != nil || len(convs) != 1 {
			t.Fatalf("default list after append = %+v, %v; want the conversation back", convs, err)
		}
	})

	t.Run("delete removes the conversation and its messages", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("delete")
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u", AssistantText: "a"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(ctx, project, "survivor", Turn{UserText: "u", AssistantText: "a"}); err != nil {
			t.Fatal(err)
		}
		// Same context id in another project: the delete must not reach it.
		if err := s.Append(ctx, project+"-other", contextID, Turn{UserText: "u", AssistantText: "a"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, project, contextID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetConversation(ctx, project, contextID); !errors.Is(err, ErrConversationNotFound) {
			t.Fatalf("get after delete err = %v, want ErrConversationNotFound", err)
		}
		if turns, err := s.Turns(ctx, project, contextID); err != nil || turns != nil {
			t.Fatalf("turns after delete = %+v, %v; want nil", turns, err)
		}
		if msgs, err := s.Messages(ctx, project, contextID); err != nil || len(msgs) != 0 {
			t.Fatalf("messages after delete = %+v, %v; want none", msgs, err)
		}
		if convs, err := s.ListConversations(ctx, project, ListOptions{Limit: 10}); err != nil || len(convs) != 1 || convs[0].ContextID != "survivor" {
			t.Fatalf("list after delete = %+v, %v; want only the survivor", convs, err)
		}
		if _, err := s.GetConversation(ctx, project+"-other", contextID); err != nil {
			t.Fatalf("delete reached across projects: %v", err)
		}
		// Irreversible, and a second delete says so rather than succeeding.
		if err := s.Delete(ctx, project, contextID); !errors.Is(err, ErrConversationNotFound) {
			t.Fatalf("second delete err = %v, want ErrConversationNotFound", err)
		}
	})

	t.Run("archive and delete of an unknown conversation are not found", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("lifecycle-missing")
		if err := s.SetArchived(ctx, project, contextID, true); !errors.Is(err, ErrConversationNotFound) {
			t.Fatalf("archive err = %v, want ErrConversationNotFound", err)
		}
		if err := s.Delete(ctx, project, contextID); !errors.Is(err, ErrConversationNotFound) {
			t.Fatalf("delete err = %v, want ErrConversationNotFound", err)
		}
		// Neither conjured the conversation on the way to failing.
		if convs, err := s.ListConversations(ctx, project, ListOptions{Limit: 10, Archived: true}); err != nil || len(convs) != 0 {
			t.Fatalf("archived list = %+v, %v; want empty", convs, err)
		}
	})

	t.Run("compact replaces stored turns with summary+keep", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact")
		for i := 1; i <= 5; i++ {
			err := s.Append(ctx, project, contextID, Turn{
				UserText:      fmt.Sprintf("u%d", i),
				AssistantText: fmt.Sprintf("a%d", i),
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		summary := NewSummaryTurn("digest of u1-u3")
		keep := []Turn{
			{UserText: "u4", AssistantText: "a4"},
			{UserText: "u5", AssistantText: "a5"},
		}
		if err := s.Compact(ctx, project, contextID, summary, 3, 0); err != nil {
			t.Fatal(err)
		}
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 3 {
			t.Fatalf("got %d turns after compact, want 3 (summary+2 keep): %+v", len(turns), turns)
		}
		if !IsSummaryTurn(turns[0]) || turns[0].AssistantText != "digest of u1-u3" {
			t.Fatalf("turns[0] = %+v, want the summary turn first", turns[0])
		}
		if turns[1] != keep[0] || turns[2] != keep[1] {
			t.Fatalf("kept turns out of order: %+v", turns[1:])
		}

		// Conversation listing reflects the new, smaller turn count.
		conv, err := s.(Reader).GetConversation(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if conv.TurnCount != 3 {
			t.Fatalf("TurnCount after compact = %d, want 3", conv.TurnCount)
		}
	})

	t.Run("compact through the newest turn leaves only the summary", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact-empty")
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u1", AssistantText: "a1"}); err != nil {
			t.Fatal(err)
		}
		summary := NewSummaryTurn("everything digested")
		if err := s.Compact(ctx, project, contextID, summary, 1, 0); err != nil {
			t.Fatal(err)
		}
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 1 || !IsSummaryTurn(turns[0]) || turns[0].AssistantText != "everything digested" {
			t.Fatalf("turns after empty-keep compact = %+v, want just the summary", turns)
		}
	})

	t.Run("compact on a fresh conversation (no prior turns)", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact-fresh")
		summary := NewSummaryTurn("digest")
		if err := s.Compact(ctx, project, contextID, summary, 0, 0); err != nil {
			t.Fatal(err)
		}
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 1 || !IsSummaryTurn(turns[0]) {
			t.Fatalf("turns after compact on fresh conversation = %+v", turns)
		}
		conv, err := s.GetConversation(ctx, project, contextID)
		if err != nil || conv.TurnCount != 1 {
			t.Fatalf("conversation after compact on fresh conversation = %+v, %v", conv, err)
		}
		// The next turn continues after the summary.
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u1", AssistantText: "a1"}); err != nil {
			t.Fatal(err)
		}
		if turns, err := s.Turns(ctx, project, contextID); err != nil || len(turns) != 2 || turns[1].UserText != "u1" {
			t.Fatalf("turns after append = %+v, %v", turns, err)
		}
	})

	t.Run("append after compact continues the sequence correctly", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact-append")
		for i := 1; i <= 4; i++ {
			err := s.Append(ctx, project, contextID, Turn{
				UserText:      fmt.Sprintf("u%d", i),
				AssistantText: fmt.Sprintf("a%d", i),
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		summary := NewSummaryTurn("digest of u1-u2")
		if err := s.Compact(ctx, project, contextID, summary, 2, 0); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u5", AssistantText: "a5"}); err != nil {
			t.Fatal(err)
		}
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 4 {
			t.Fatalf("got %d turns, want 4 (summary + 2 kept + 1 appended): %+v", len(turns), turns)
		}
		if !IsSummaryTurn(turns[0]) {
			t.Fatalf("turns[0] should still be the summary: %+v", turns[0])
		}
		last := turns[len(turns)-1]
		if last.UserText != "u5" || last.AssistantText != "a5" {
			t.Fatalf("appended turn after compact = %+v, want u5/a5", last)
		}
	})

	t.Run("Messages renders a summary turn distinctly", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("messages-summary")
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u1", AssistantText: "a1"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u2", AssistantText: "a2"}); err != nil {
			t.Fatal(err)
		}
		summary := NewSummaryTurn("digest of u1")
		if err := s.Compact(ctx, project, contextID, summary, 1, 0); err != nil {
			t.Fatal(err)
		}

		msgs, err := s.Messages(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		// Expect exactly 3 messages: 1 summary row + 1 user + 1 assistant for
		// the kept turn — never a spurious user-role row for the summary
		// turn's internal marker text.
		if len(msgs) != 3 {
			t.Fatalf("got %d messages, want 3 (summary + user + assistant): %+v", len(msgs), msgs)
		}
		if msgs[0].Role != "summary" || msgs[0].Content != "digest of u1" {
			t.Fatalf("msgs[0] = %+v, want Role summary with the digest content", msgs[0])
		}
		for _, m := range msgs {
			if m.Content == summaryUserMarker {
				t.Fatalf("summary turn's internal marker leaked into a rendered message: %+v", msgs)
			}
		}
		if msgs[1].Role != "user" || msgs[1].Content != "u2" {
			t.Fatalf("msgs[1] = %+v, want the kept turn's user message", msgs[1])
		}
		if msgs[2].Role != "assistant" || msgs[2].Content != "a2" {
			t.Fatalf("msgs[2] = %+v, want the kept turn's assistant message", msgs[2])
		}
		// seq stays a dense, monotonically increasing 1-based index.
		for i, m := range msgs {
			if m.Seq != int64(i+1) {
				t.Fatalf("msgs[%d].Seq = %d, want %d (dense monotonic seq)", i, m.Seq, i+1)
			}
		}
	})

	t.Run("window head is the newest turn's position", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("window-head")
		if w, err := s.Window(ctx, project, contextID); err != nil || w.Head != 0 || w.Turns != nil {
			t.Fatalf("empty window = %+v, %v", w, err)
		}
		appendTurns(t, s, project, contextID, 1, 3)
		w, err := s.Window(ctx, project, contextID)
		if err != nil || w.Head != 3 || len(w.Turns) != 3 {
			t.Fatalf("window = %+v, %v; want head 3 over 3 turns", w, err)
		}
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), 2, 0); err != nil {
			t.Fatal(err)
		}
		w, err = s.Window(ctx, project, contextID)
		if err != nil || w.Head != 2 || len(w.Turns) != 2 || w.Turns[1].UserText != "u3" {
			t.Fatalf("window after compact = %+v, %v; want summary then u3 at head 2", w, err)
		}
	})

	t.Run("a turn appended while compacting survives", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact-inflight")
		appendTurns(t, s, project, contextID, 1, 4)
		// The agent reads the window and starts summarizing u1-u2 ...
		w, err := s.Window(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		// ... another turn of the conversation finishes meanwhile ...
		appendTurns(t, s, project, contextID, 5, 5)
		// ... and the summary lands, naming the turns it was built from.
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest of u1-u2"), w.Head-2, w.Compactions); err != nil {
			t.Fatal(err)
		}
		appendTurns(t, s, project, contextID, 6, 6)

		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if got := userTexts(turns); got != "[conversation summary] u3 u4 u5 u6" {
			t.Fatalf("turns after in-flight compaction = %q", got)
		}
		if conv, err := s.GetConversation(ctx, project, contextID); err != nil || conv.TurnCount != 5 {
			t.Fatalf("conversation = %+v, %v; want TurnCount 5", conv, err)
		}
	})

	t.Run("appends racing a compaction are all kept", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact-race")
		appendTurns(t, s, project, contextID, 1, 3)
		w, err := s.Window(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		const n = 8
		var wg sync.WaitGroup
		errs := make(chan error, n+1)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- s.Append(ctx, project, contextID, Turn{
					UserText: fmt.Sprintf("r%d", i), AssistantText: "a"})
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), w.Head-1, w.Compactions)
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		// summary + u3 (kept) + every racing append, none torn or lost.
		if len(turns) != 2+n || !IsSummaryTurn(turns[0]) || turns[1].UserText != "u3" {
			t.Fatalf("turns after racing compaction = %q", userTexts(turns))
		}
		seen := map[string]bool{}
		for _, turn := range turns[2:] {
			if turn.AssistantText != "a" {
				t.Fatalf("torn turn: %+v", turn)
			}
			seen[turn.UserText] = true
		}
		if len(seen) != n {
			t.Fatalf("racing appends lost: %q", userTexts(turns))
		}
	})

	t.Run("compact past the newest turn changes nothing", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact-range")
		appendTurns(t, s, project, contextID, 1, 2)
		for _, through := range []int64{-1, 3} {
			err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), through, 0)
			if !errors.Is(err, ErrCompactRange) {
				t.Fatalf("Compact(through=%d) = %v, want ErrCompactRange", through, err)
			}
		}
		if turns, err := s.Turns(ctx, project, contextID); err != nil || userTexts(turns) != "u1 u2" {
			t.Fatalf("turns after rejected compaction = %q, %v", userTexts(turns), err)
		}
		if _, err := s.GetConversation(ctx, project, "ctx-never-seen-"+uniqueSuffix(t)); !errors.Is(err, ErrConversationNotFound) {
			t.Fatalf("unknown conversation = %v", err)
		}
		if err := s.Compact(ctx, project, "ctx-range-fresh-"+uniqueSuffix(t), NewSummaryTurn("d"), 1, 0); !errors.Is(err, ErrCompactRange) {
			t.Fatalf("Compact(through=1) on a fresh conversation = %v, want ErrCompactRange", err)
		}
	})

	t.Run("a second compaction from the same window is refused", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact-stale")
		appendTurns(t, s, project, contextID, 1, 6)
		// Two replicas read the same window and both decide to compact.
		w, err := s.Window(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		// A summarizes u1-u2 and commits first, renumbering the turns.
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest A"), w.Positions[1], w.Compactions); err != nil {
			t.Fatal(err)
		}
		// B summarized only u1; its through is a position in the numbering
		// A just replaced, where it would name A's summary.
		err = s.Compact(ctx, project, contextID, NewSummaryTurn("digest B"), w.Positions[0], w.Compactions)
		if !errors.Is(err, ErrCompactStale) {
			t.Fatalf("second Compact = %v, want ErrCompactStale", err)
		}
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if got := userTexts(turns); got != "[conversation summary] u3 u4 u5 u6" || turns[0].AssistantText != "digest A" {
			t.Fatalf("turns after refused compaction = %q (%q)", got, turns[0].AssistantText)
		}
		if conv, err := s.GetConversation(ctx, project, contextID); err != nil || conv.TurnCount != 5 {
			t.Fatalf("conversation = %+v, %v; want TurnCount 5", conv, err)
		}
		// A fresh read sees the new count and compacts again.
		w, err = s.Window(ctx, project, contextID)
		if err != nil || w.Compactions != 1 {
			t.Fatalf("window after compaction = %+v, %v; want Compactions 1", w, err)
		}
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest C"), w.Positions[1], w.Compactions); err != nil {
			t.Fatal(err)
		}
		if turns, err := s.Turns(ctx, project, contextID); err != nil || userTexts(turns) != "[conversation summary] u4 u5 u6" {
			t.Fatalf("turns after re-read compaction = %q, %v", userTexts(turns), err)
		}
	})

	t.Run("a compaction after an append still applies", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact-after-append")
		appendTurns(t, s, project, contextID, 1, 3)
		w, err := s.Window(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		// An append does not renumber, so it does not make the window stale.
		appendTurns(t, s, project, contextID, 4, 4)
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), w.Positions[0], w.Compactions); err != nil {
			t.Fatalf("Compact after append = %v", err)
		}
		if turns, err := s.Turns(ctx, project, contextID); err != nil || userTexts(turns) != "[conversation summary] u2 u3 u4" {
			t.Fatalf("turns = %q, %v", userTexts(turns), err)
		}
	})

	t.Run("window positions hold across an empty turn", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("window-gap")
		appendTurns(t, s, project, contextID, 1, 2)
		if err := s.Append(ctx, project, contextID, Turn{}); err != nil {
			t.Fatal(err)
		}
		appendTurns(t, s, project, contextID, 4, 5)
		w, err := s.Window(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		// A store may leave the empty turn out; either way every returned
		// turn carries its own position and the newest is the head.
		if len(w.Positions) != len(w.Turns) || w.Head != 5 || w.Positions[len(w.Positions)-1] != 5 {
			t.Fatalf("window = %+v; want one position per turn, head 5", w)
		}
		for i, turn := range w.Turns {
			if turn.UserText != "" && turn.UserText != fmt.Sprintf("u%d", w.Positions[i]) {
				t.Fatalf("turn %q reported at position %d", turn.UserText, w.Positions[i])
			}
		}
		// Keep the newest two turns: compact through the turn before u4.
		keepFrom := len(w.Turns) - 2
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), w.Positions[keepFrom]-1, w.Compactions); err != nil {
			t.Fatal(err)
		}
		if turns, err := s.Turns(ctx, project, contextID); err != nil || userTexts(turns) != "[conversation summary] u4 u5" {
			t.Fatalf("turns after compaction across a gap = %q, %v", userTexts(turns), err)
		}
	})

	t.Run("concurrent appends never collide", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("conc")
		const n = 10
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs <- s.Append(ctx, project, contextID, Turn{
					UserText:      fmt.Sprintf("u%d", i),
					AssistantText: fmt.Sprintf("a%d", i),
				})
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != n {
			t.Fatalf("got %d turns, want %d", len(turns), n)
		}
		for _, turn := range turns {
			if turn.UserText == "" || turn.AssistantText == "" {
				t.Fatalf("torn turn after concurrent appends: %+v", turn)
			}
		}
	})
}

// appendTurns appends turns u<from>..u<to> (answers a<i>) in order.
func appendTurns(t *testing.T, s Store, project, contextID string, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		turn := Turn{UserText: fmt.Sprintf("u%d", i), AssistantText: fmt.Sprintf("a%d", i)}
		if err := s.Append(context.Background(), project, contextID, turn); err != nil {
			t.Fatal(err)
		}
	}
}

// userTexts joins the turns' user texts with spaces, for compact assertions.
func userTexts(turns []Turn) string {
	parts := make([]string, len(turns))
	for i, turn := range turns {
		parts[i] = turn.UserText
	}
	return strings.Join(parts, " ")
}

var uniqueCounter struct {
	sync.Mutex
	n int
}

// uniqueSuffix gives per-invocation unique identifiers WITHOUT time/random so
// runs against a persistent shared database never collide within a process,
// and the test name namespaces across processes via os.Getpid.
func uniqueSuffix(t *testing.T) string {
	t.Helper()
	uniqueCounter.Lock()
	defer uniqueCounter.Unlock()
	uniqueCounter.n++
	return fmt.Sprintf("%d-%d", os.Getpid(), uniqueCounter.n)
}

func TestMemoryStoreConformance(t *testing.T) {
	storeConformance(t, func(t *testing.T) interface {
		Store
		Editor
	} {
		return NewMemoryStore()
	})
}

func TestPostgresStoreConformance(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres store tests (memory conformance still ran)")
	}
	storeConformance(t, func(t *testing.T) interface {
		Store
		Editor
	} {
		s, err := NewPostgresStore(context.Background(), url, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(s.Close)
		return s
	})
}

func TestPostgresStoreBadURLFailsFast(t *testing.T) {
	_, err := NewPostgresStore(context.Background(), "postgres://nobody@127.0.0.1:1/nope?connect_timeout=1", nil)
	if err == nil {
		t.Fatal("want an error for an unreachable database (no silent fallback)")
	}
}
