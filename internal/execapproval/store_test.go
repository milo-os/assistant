package execapproval

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/milo-os/assistant/internal/testdb"
)

func TestMemoryStoreSingleUseAcrossConcurrentCalls(t *testing.T) {
	testSingleUse(t, NewMemoryStore())
}

func TestMemoryStoreRelease(t *testing.T) {
	testRelease(t, NewMemoryStore())
}

func TestPostgresStoreSingleUseAndPruning(t *testing.T) {
	url := testdb.URL(t)
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	testSingleUse(t, store)

	project := fmt.Sprintf("exec-approval-prune-%d", time.Now().UnixNano())
	old := MakeKey(project, "expired-plan")
	if _, err := store.pool.Exec(ctx, `INSERT INTO assistant_exec_approvals
		(project, plan_hash, consumed_at) VALUES ($1, $2, now() - interval '2 days')`,
		project, old.Plan[:]); err != nil {
		t.Fatal(err)
	}
	rows := func() int {
		t.Helper()
		var count int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM assistant_exec_approvals
			WHERE project = $1 AND plan_hash = $2`, project, old.Plan[:]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	// testSingleUse already pruned, so within the interval Consume leaves the
	// expired row alone.
	if used, err := store.Consume(ctx, project, "within-interval"); err != nil || !used {
		t.Fatalf("consume within the interval: used=%v err=%v", used, err)
	}
	if rows() != 1 {
		t.Fatal("Consume pruned again within the prune interval")
	}

	// Once the interval has passed, the next Consume prunes.
	clock := time.Now().Add(pruneInterval)
	store.prune.now = func() time.Time { return clock }
	if used, err := store.Consume(ctx, project, "new-plan"); err != nil || !used {
		t.Fatalf("consume after pruning: used=%v err=%v", used, err)
	}
	if rows() != 0 {
		t.Fatal("expired approval row was not pruned")
	}

	testRelease(t, store)
	testRelease(t, store.Retaining())

	// A retained approval outlives the prune: a day-old spent plan approval
	// must still refuse a replay.
	retained := store.Retaining()
	if used, err := retained.Consume(ctx, project, "retained-plan"); err != nil || !used {
		t.Fatalf("consume retained: used=%v err=%v", used, err)
	}
	kept := MakeKey(project, "retained-plan")
	if _, err := store.pool.Exec(ctx, `UPDATE assistant_exec_approvals
		SET consumed_at = now() - interval '30 days' WHERE project = $1 AND plan_hash = $2`,
		project, kept.Plan[:]); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(pruneInterval)
	if _, err := store.Consume(ctx, project, "prune-trigger"); err != nil {
		t.Fatal(err)
	}
	if used, err := retained.Consume(ctx, project, "retained-plan"); err != nil || used {
		t.Fatalf("a retained approval was pruned: used=%v err=%v", used, err)
	}
}

// TestPostgresStoreIndexesOnlyPrunableRows pins the migrate-on-open step: a
// table that still has the old full index on consumed_at ends up with only the
// partial index over rows the prune may delete, and opening again is a no-op.
func TestPostgresStoreIndexesOnlyPrunableRows(t *testing.T) {
	url := testdb.URL(t)
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS assistant_exec_approvals_consumed_at_idx
		ON assistant_exec_approvals (consumed_at)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reopened, err := NewPostgresStore(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		reopened.Close()
	}

	indexes := map[string]string{}
	rows, err := store.pool.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes
		WHERE tablename = 'assistant_exec_approvals' AND indexdef LIKE '%consumed_at%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		indexes[name] = def
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 1 {
		t.Fatalf("indexes on consumed_at = %v, want only the partial one", indexes)
	}
	def, ok := indexes["assistant_exec_approvals_prunable_idx"]
	if !ok || !strings.Contains(def, "WHERE (NOT retain)") {
		t.Fatalf("prune index = %q, want a partial index WHERE NOT retain", def)
	}
}

// testRelease checks that a released approval can be consumed again, once,
// and that releasing one never consumed is harmless.
func testRelease(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	project := fmt.Sprintf("exec-approval-release-%d", time.Now().UnixNano())
	if err := store.Release(ctx, project, "never-consumed"); err != nil {
		t.Fatalf("release of an unconsumed approval: %v", err)
	}
	if used, err := store.Consume(ctx, project, "plan"); err != nil || !used {
		t.Fatalf("first consume: used=%v err=%v", used, err)
	}
	if err := store.Release(ctx, project, "plan"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if used, err := store.Consume(ctx, project, "plan"); err != nil || !used {
		t.Fatalf("consume after release: used=%v err=%v", used, err)
	}
	if used, err := store.Consume(ctx, project, "plan"); err != nil || used {
		t.Fatalf("consume twice after release: used=%v err=%v", used, err)
	}
}

func testSingleUse(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	project := fmt.Sprintf("exec-approval-%d", time.Now().UnixNano())
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			used, err := store.Consume(ctx, project, "same-plan")
			if err != nil {
				t.Errorf("consume: %v", err)
				return
			}
			results <- used
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for used := range results {
		if used {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful consumes = %d, want exactly one", successes)
	}
}
