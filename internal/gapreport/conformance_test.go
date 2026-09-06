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
		r, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "streaming.streamco.example",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "list pipelines", Summary: "user needed a pipeline id"})
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
		if _, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "cap", Summary: "summary"}); err != nil {
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
		if _, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "consumer-a", ContextID: "ctx-1", Capability: "cap", Summary: "summary"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "consumer-b", ContextID: "ctx-2", Capability: "cap", Summary: "summary"}); err != nil {
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
		if _, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "first", Summary: "s"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "second", Summary: "s"}); err != nil {
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
		_, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: strings.Repeat("x", MaxCapabilityLen+1), Summary: "s"})
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
		_, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "cap", Summary: strings.Repeat("x", MaxSummaryLen+1)})
		if !errors.Is(err, ErrSummaryTooLong) {
			t.Fatalf("Insert = %v; want ErrSummaryTooLong", err)
		}
	})

	t.Run("provider project full rejects further inserts", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("full")
		for i := range MaxReportsPerProject {
			if _, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
				ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: fmt.Sprintf("cap%d", i), Summary: "s"}); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}
		if _, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "one-too-many", Summary: "s"}); !errors.Is(err, ErrProjectFull) {
			t.Fatalf("Insert over cap = %v; want ErrProjectFull", err)
		}
	})

	t.Run("kind omitted defaults to MissingCapability", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("defaultkind")
		r, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "cap", Summary: "s"})
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind != KindMissingCapability {
			t.Fatalf("Insert returned Kind = %q; want %q", r.Kind, KindMissingCapability)
		}
		reports, err := s.List(ctx, provider)
		if err != nil || len(reports) != 1 {
			t.Fatalf("List = %v, %v; want 1", reports, err)
		}
		if reports[0].Kind != KindMissingCapability {
			t.Fatalf("stored Kind = %q; want %q", reports[0].Kind, KindMissingCapability)
		}
		if !reports[0].Evidence.IsZero() {
			t.Fatalf("Evidence = %+v; want zero for a report filed without any", reports[0].Evidence)
		}
	})

	t.Run("every kind round-trips with its evidence", func(t *testing.T) {
		for _, kind := range Kinds {
			s := newStore(t)
			provider := fresh("kind-" + string(kind))
			want := Evidence{
				Tool:           "workloads_list",
				Observed:       "actionability: transient",
				ContradictedBy: "instance unchanged for 9d",
			}
			if _, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
				ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "cap", Summary: "s",
				Kind: kind, Evidence: want}); err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			reports, err := s.List(ctx, provider)
			if err != nil || len(reports) != 1 {
				t.Fatalf("%s: List = %v, %v; want 1", kind, reports, err)
			}
			if reports[0].Kind != kind {
				t.Fatalf("Kind = %q; want %q", reports[0].Kind, kind)
			}
			if reports[0].Evidence != want {
				t.Fatalf("%s: Evidence = %+v; want %+v", kind, reports[0].Evidence, want)
			}
		}
	})

	t.Run("unknown kind is rejected", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("badkind")
		_, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "cap", Summary: "s",
			Kind: Kind("SlightlyOff")})
		if !errors.Is(err, ErrUnknownKind) {
			t.Fatalf("Insert = %v; want ErrUnknownKind", err)
		}
		reports, _ := s.List(ctx, provider)
		if len(reports) != 0 {
			t.Fatal("rejected insert must not write a partial report")
		}
	})

	// The deliberate choice: an under-evidenced report still names the tool
	// and the kind, which is actionable; rejecting it would drop the signal
	// and push the caller toward relabelling it MissingCapability, which is
	// a worse record than a thin one. The nudge lives in the tool result.
	t.Run("a kind that wants evidence is still accepted without it", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("noevidence")
		r, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "cap", Summary: "s",
			Kind: KindMisleadingOutput})
		if err != nil {
			t.Fatalf("Insert = %v; a kind with no evidence must be accepted, not rejected", err)
		}
		if r.Kind != KindMisleadingOutput || !r.Evidence.IsZero() {
			t.Fatalf("unexpected report: %+v", r)
		}
		reports, err := s.List(ctx, provider)
		if err != nil || len(reports) != 1 || reports[0].Kind != KindMisleadingOutput {
			t.Fatalf("List = %v, %v; want the report stored as %s", reports, err, KindMisleadingOutput)
		}
	})

	t.Run("evidence fields over their bounds are rejected", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			evidence Evidence
		}{
			{"tool", Evidence{Tool: strings.Repeat("x", MaxEvidenceToolLen+1)}},
			{"observed", Evidence{Observed: strings.Repeat("x", MaxEvidenceTextLen+1)}},
			{"contradictedBy", Evidence{ContradictedBy: strings.Repeat("x", MaxEvidenceTextLen+1)}},
		} {
			s := newStore(t)
			provider := fresh("evbig-" + tc.name)
			_, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
				ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "cap", Summary: "s",
				Kind: KindInsufficientDetail, Evidence: tc.evidence})
			if !errors.Is(err, ErrEvidenceTooLong) {
				t.Fatalf("%s: Insert = %v; want ErrEvidenceTooLong", tc.name, err)
			}
			reports, _ := s.List(ctx, provider)
			if len(reports) != 0 {
				t.Fatalf("%s: rejected insert must not write a partial report", tc.name)
			}
		}
	})

	t.Run("evidence at exactly its bound is accepted", func(t *testing.T) {
		s := newStore(t)
		provider := fresh("evexact")
		if _, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
			ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "cap", Summary: "s",
			Kind: KindMisleadingOutput, Evidence: Evidence{
				Tool:           strings.Repeat("x", MaxEvidenceToolLen),
				Observed:       strings.Repeat("y", MaxEvidenceTextLen),
				ContradictedBy: strings.Repeat("z", MaxEvidenceTextLen),
			}}); err != nil {
			t.Fatalf("Insert at the bound = %v; want accepted", err)
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
				_, err := s.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
					ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: fmt.Sprintf("cap%d", i), Summary: "s"})
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

// TestPostgresStoreLegacyRowReadsBackAsMissingCapability covers the one case
// no in-memory store can reproduce: a row written before the kind and
// evidence columns existed. Inserting with only the original seven columns
// leaves the new ones at their ALTER TABLE defaults, which is exactly the
// state the migration leaves every pre-existing row in.
func TestPostgresStoreLegacyRowReadsBackAsMissingCapability(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres store tests (memory conformance still ran)")
	}
	ctx := context.Background()
	s, err := NewPostgresStore(ctx, url, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	provider := "provider-legacy-" + uniqueSuffix(t)
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO capability_gap_report
		   (id, provider_project, service_name, consumer_project, context_id, capability, summary)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		newReportID(), provider, "svc", "demo-project", "ctx-1", "list pipelines", "user needed a pipeline id",
	); err != nil {
		t.Fatalf("insert legacy-shaped row: %v", err)
	}

	reports, err := s.List(ctx, provider)
	if err != nil || len(reports) != 1 {
		t.Fatalf("List = %v, %v; want 1", reports, err)
	}
	if reports[0].Kind != KindMissingCapability {
		t.Fatalf("legacy row Kind = %q; want %q", reports[0].Kind, KindMissingCapability)
	}
	if !reports[0].Evidence.IsZero() {
		t.Fatalf("legacy row Evidence = %+v; want zero", reports[0].Evidence)
	}
	if reports[0].Capability != "list pipelines" || reports[0].Summary != "user needed a pipeline id" {
		t.Fatalf("legacy row lost its content: %+v", reports[0])
	}
}

// TestPostgresStoreSchemaIsIdempotent re-applies the schema against a
// database that already has it — the migrate-on-open path every process
// start takes against the shared staging database.
func TestPostgresStoreSchemaIsIdempotent(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres store tests (memory conformance still ran)")
	}
	ctx := context.Background()
	first, err := NewPostgresStore(ctx, url, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(first.Close)

	provider := "provider-idem-" + uniqueSuffix(t)
	want := Evidence{Tool: "workloads_list", Observed: "actionability: transient", ContradictedBy: "unchanged for 9d"}
	if _, err := first.Insert(ctx, InsertParams{ProviderProject: provider, ServiceName: "svc",
		ConsumerProject: "demo-project", ContextID: "ctx-1", Capability: "cap", Summary: "s",
		Kind: KindMisleadingOutput, Evidence: want}); err != nil {
		t.Fatal(err)
	}

	// Opening again re-runs every schema statement, including the ALTERs.
	second, err := NewPostgresStore(ctx, url, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("re-applying the schema must succeed and must not disturb data: %v", err)
	}
	t.Cleanup(second.Close)

	reports, err := second.List(ctx, provider)
	if err != nil || len(reports) != 1 {
		t.Fatalf("List after re-open = %v, %v; want 1", reports, err)
	}
	if reports[0].Kind != KindMisleadingOutput || reports[0].Evidence != want {
		t.Fatalf("re-applying the schema changed stored data: %+v", reports[0])
	}
}

func TestPostgresStoreBadURLFailsFast(t *testing.T) {
	_, err := NewPostgresStore(context.Background(), "postgres://nobody@127.0.0.1:1/nope?connect_timeout=1", nil)
	if err == nil {
		t.Fatal("want an error for an unreachable database (no silent fallback)")
	}
}
