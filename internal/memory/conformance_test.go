package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
)

// storeConformance runs the behavioral contract every [Store] must satisfy.
// MemoryStore always runs it; PostgresStore runs it against TEST_DATABASE_URL
// when set (skipped otherwise), matching internal/history's pattern.
func storeConformance(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()

	fresh := func(name string) string {
		return "proj-" + name + "-" + uniqueSuffix(t)
	}

	t.Run("round-trip", func(t *testing.T) {
		s := newStore(t)
		project := fresh("rt")
		if err := s.Upsert(ctx, project, "goal", "ship the memory feature"); err != nil {
			t.Fatal(err)
		}
		f, ok, err := s.Get(ctx, project, "goal")
		if err != nil || !ok || f.Value != "ship the memory feature" {
			t.Fatalf("Get = %+v, %v, %v", f, ok, err)
		}
	})

	t.Run("unknown project and key are empty, not error", func(t *testing.T) {
		s := newStore(t)
		project := fresh("empty")
		facts, err := s.List(ctx, project)
		if err != nil || facts != nil {
			t.Fatalf("List = %v, %v; want nil, nil", facts, err)
		}
		_, ok, err := s.Get(ctx, project, "nope")
		if err != nil || ok {
			t.Fatalf("Get = ok=%v, %v; want false, nil", ok, err)
		}
	})

	t.Run("project isolation", func(t *testing.T) {
		s := newStore(t)
		project := fresh("iso")
		if err := s.Upsert(ctx, project, "secret", "value"); err != nil {
			t.Fatal(err)
		}
		_, ok, err := s.Get(ctx, project+"-other", "secret")
		if err != nil || ok {
			t.Fatalf("cross-project get = ok=%v, %v; want false, nil", ok, err)
		}
		facts, err := s.List(ctx, project+"-other")
		if err != nil || len(facts) != 0 {
			t.Fatalf("cross-project list = %v, %v; want empty", facts, err)
		}
	})

	t.Run("upsert replaces existing key", func(t *testing.T) {
		s := newStore(t)
		project := fresh("replace")
		if err := s.Upsert(ctx, project, "k", "v1"); err != nil {
			t.Fatal(err)
		}
		if err := s.Upsert(ctx, project, "k", "v2"); err != nil {
			t.Fatal(err)
		}
		f, ok, err := s.Get(ctx, project, "k")
		if err != nil || !ok || f.Value != "v2" {
			t.Fatalf("Get = %+v, %v, %v; want v2", f, ok, err)
		}
		facts, err := s.List(ctx, project)
		if err != nil || len(facts) != 1 {
			t.Fatalf("List = %v, %v; want 1 fact", facts, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		s := newStore(t)
		project := fresh("del")
		if err := s.Upsert(ctx, project, "k", "v"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, project, "k"); err != nil {
			t.Fatal(err)
		}
		_, ok, err := s.Get(ctx, project, "k")
		if err != nil || ok {
			t.Fatalf("Get after delete = ok=%v, %v; want false, nil", ok, err)
		}
		// Deleting an unset key is a no-op, not an error.
		if err := s.Delete(ctx, project, "never-existed"); err != nil {
			t.Fatalf("Delete unset key = %v; want nil", err)
		}
	})

	t.Run("value too long is rejected", func(t *testing.T) {
		s := newStore(t)
		project := fresh("toolong")
		err := s.Upsert(ctx, project, "k", strings.Repeat("x", MaxFactValueLen+1))
		if !errors.Is(err, ErrValueTooLong) {
			t.Fatalf("Upsert = %v; want ErrValueTooLong", err)
		}
		_, ok, _ := s.Get(ctx, project, "k")
		if ok {
			t.Fatal("rejected upsert must not write a partial fact")
		}
	})

	t.Run("project full rejects a new key but allows updating existing ones", func(t *testing.T) {
		s := newStore(t)
		project := fresh("full")
		for i := range MaxFactsPerProject {
			if err := s.Upsert(ctx, project, fmt.Sprintf("k%d", i), "v"); err != nil {
				t.Fatalf("upsert %d: %v", i, err)
			}
		}
		if err := s.Upsert(ctx, project, "one-too-many", "v"); !errors.Is(err, ErrProjectFull) {
			t.Fatalf("Upsert over cap = %v; want ErrProjectFull", err)
		}
		// Updating an existing key must still work even at the cap.
		if err := s.Upsert(ctx, project, "k0", "updated"); err != nil {
			t.Fatalf("update existing key at cap: %v", err)
		}
	})

	t.Run("concurrent upserts to the same key never collide", func(t *testing.T) {
		s := newStore(t)
		project := fresh("conc")
		const n = 10
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs <- s.Upsert(ctx, project, "k", fmt.Sprintf("v%d", i))
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		facts, err := s.List(ctx, project)
		if err != nil || len(facts) != 1 {
			t.Fatalf("List = %v, %v; want exactly 1 fact after concurrent upserts to one key", facts, err)
		}
	})
}

var uniqueCounter struct {
	sync.Mutex
	n int
}

// uniqueSuffix gives per-invocation unique identifiers without time/random so
// runs against a persistent shared database never collide within a process.
func uniqueSuffix(t *testing.T) string {
	t.Helper()
	uniqueCounter.Lock()
	defer uniqueCounter.Unlock()
	uniqueCounter.n++
	return fmt.Sprintf("%d-%d", os.Getpid(), uniqueCounter.n)
}

func TestMemoryStoreConformance(t *testing.T) {
	storeConformance(t, func(t *testing.T) Store {
		return NewMemoryStore()
	})
}

func TestPostgresStoreConformance(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres store tests (memory conformance still ran)")
	}
	storeConformance(t, func(t *testing.T) Store {
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
