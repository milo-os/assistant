package history

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
)

// storeConformance runs the behavioral contract every Store+Lister must
// satisfy. MemoryStore always runs it; PostgresStore runs it against
// TEST_DATABASE_URL when set (skipped otherwise, so `go test ./...` needs no
// database). The Postgres e2e path additionally proves restart survival —
// that cannot be expressed here.
func storeConformance(t *testing.T, newStore func(t *testing.T) interface {
	Store
	Lister
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
		convs, err := s.ListConversations(ctx, project+"-other", 10)
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
		convs, err := s.ListConversations(ctx, project, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(convs) != 2 || convs[0].ContextID != "c1" || convs[0].TurnCount != 2 || convs[1].TurnCount != 1 {
			t.Fatalf("unexpected listing: %+v", convs)
		}
		if convs[0].CreatedAt.IsZero() || convs[0].LastActiveAt.Before(convs[0].CreatedAt) {
			t.Fatalf("bad timestamps: %+v", convs[0])
		}
		limited, err := s.ListConversations(ctx, project, 1)
		if err != nil || len(limited) != 1 {
			t.Fatalf("limit not honored: %v, %v", limited, err)
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
		Lister
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
		Lister
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
