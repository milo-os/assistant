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
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"),
			[]Turn{{UserText: "and the edge-cache one?", AssistantText: "a"}}); err != nil {
			t.Fatal(err)
		}
		got, err = s.GetConversation(ctx, project, contextID)
		if err != nil || got.Title != "and the edge-cache one?" {
			t.Fatalf("post-compact title = %q, %v", got.Title, err)
		}
		// Only a summary left: no title rather than the marker text.
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), nil); err != nil {
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
		if err := s.Compact(ctx, project, contextID, NewSummaryTurn("digest"), nil); err != nil {
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
		if err := s.Compact(ctx, project, contextID, summary, keep); err != nil {
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

	t.Run("compact with empty keep leaves only the summary", func(t *testing.T) {
		s := newStore(t)
		project, contextID := fresh("compact-empty")
		if err := s.Append(ctx, project, contextID, Turn{UserText: "u1", AssistantText: "a1"}); err != nil {
			t.Fatal(err)
		}
		summary := NewSummaryTurn("everything digested")
		if err := s.Compact(ctx, project, contextID, summary, nil); err != nil {
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
		keep := []Turn{{UserText: "u1", AssistantText: "a1"}}
		if err := s.Compact(ctx, project, contextID, summary, keep); err != nil {
			t.Fatal(err)
		}
		turns, err := s.Turns(ctx, project, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 2 || !IsSummaryTurn(turns[0]) || turns[1] != keep[0] {
			t.Fatalf("turns after compact on fresh conversation = %+v", turns)
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
		keep := []Turn{{UserText: "u3", AssistantText: "a3"}, {UserText: "u4", AssistantText: "a4"}}
		if err := s.Compact(ctx, project, contextID, summary, keep); err != nil {
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
		keep := []Turn{{UserText: "u2", AssistantText: "a2"}}
		if err := s.Compact(ctx, project, contextID, summary, keep); err != nil {
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
