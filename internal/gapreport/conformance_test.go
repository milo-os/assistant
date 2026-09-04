package gapreport

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
// when set (skipped otherwise), matching internal/memory's pattern.
func storeConformance(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()

	fresh := func(name string) string {
		return "provider-" + name + "-" + uniqueSuffix(t)
	}

	t.Run("round-trip", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("rt")
		r, err := s.Insert(ctx, provider, "streaming.streamco.example", "demo-project", "ctx-1", "list pipelines", "user needed a pipeline id")
		if err != nil {
			t.Fatal(err)
		}
		if r.ID == "" {
			t.Fatal("Insert must assign an ID")
		}
		if r.CreatedAt.IsZero() {
			t.Fatal("Insert must assign CreatedAt")
		}
		reports, err := s.List(ctx, provider)
		if err != nil || len(reports) != 1 || reports[0].Capability != "list pipelines" {
			t.Fatalf("List = %+v, %v", reports, err)
		}
	})

	t.Run("unknown provider project is empty, not error", func(t *testing.T) {
		s := newStore(t)
		reports, err := s.List(ctx, fresh("empty"))
		if err != nil || reports != nil {
			t.Fatalf("List = %v, %v; want nil, nil", reports, err)
		}
	})

	t.Run("provider project isolation", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("iso")
		if _, err := s.Insert(ctx, provider, "svc", "demo-project", "ctx-1", "cap", "summary"); err != nil {
			t.Fatal(err)
		}
		reports, err := s.List(ctx, provider+"-other")
		if err != nil || len(reports) != 0 {
			t.Fatalf("cross-provider list = %v, %v; want empty", reports, err)
		}
	})

	t.Run("consumer project attribution does not affect the write key", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("attr")
		if _, err := s.Insert(ctx, provider, "svc", "consumer-a", "ctx-1", "cap", "summary"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Insert(ctx, provider, "svc", "consumer-b", "ctx-2", "cap", "summary"); err != nil {
			t.Fatal(err)
		}
		reports, err := s.List(ctx, provider)
		if err != nil || len(reports) != 2 {
			t.Fatalf("List = %v, %v; want 2 reports from different consumer projects, same provider", reports, err)
		}
	})

	t.Run("newest first", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("order")
		if _, err := s.Insert(ctx, provider, "svc", "demo-project", "ctx-1", "first", "s"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Insert(ctx, provider, "svc", "demo-project", "ctx-1", "second", "s"); err != nil {
			t.Fatal(err)
		}
		reports, err := s.List(ctx, provider)
		if err != nil || len(reports) != 2 {
			t.Fatalf("List = %v, %v; want 2", reports, err)
		}
		if reports[0].Capability != "second" {
			t.Fatalf("List[0].Capability = %q; want newest (%q) first", reports[0].Capability, "second")
		}
	})

	t.Run("capability too long is rejected", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("capbig")
		_, err := s.Insert(ctx, provider, "svc", "demo-project", "ctx-1", strings.Repeat("x", MaxCapabilityLen+1), "s")
		if !errors.Is(err, ErrCapabilityTooLong) {
			t.Fatalf("Insert = %v; want ErrCapabilityTooLong", err)
		}
		reports, _ := s.List(ctx, provider)
		if len(reports) != 0 {
			t.Fatal("rejected insert must not write a partial report")
		}
	})

	t.Run("summary too long is rejected", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("sumbig")
		_, err := s.Insert(ctx, provider, "svc", "demo-project", "ctx-1", "cap", strings.Repeat("x", MaxSummaryLen+1))
		if !errors.Is(err, ErrSummaryTooLong) {
			t.Fatalf("Insert = %v; want ErrSummaryTooLong", err)
		}
	})

	t.Run("provider project full rejects further inserts", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("full")
		for i := range MaxReportsPerProject {
			if _, err := s.Insert(ctx, provider, "svc", "demo-project", "ctx-1", fmt.Sprintf("cap%d", i), "s"); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}
		if _, err := s.Insert(ctx, provider, "svc", "demo-project", "ctx-1", "one-too-many", "s"); !errors.Is(err, ErrProjectFull) {
			t.Fatalf("Insert over cap = %v; want ErrProjectFull", err)
		}
	})

	t.Run("concurrent inserts never collide or lose a report", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("conc")
		const n = 10
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := s.Insert(ctx, provider, "svc", "demo-project", "ctx-1", fmt.Sprintf("cap%d", i), "s")
				errs <- err
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		reports, err := s.List(ctx, provider)
		if err != nil || len(reports) != n {
			t.Fatalf("List = %v, %v; want exactly %d reports after concurrent inserts", reports, err, n)
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
