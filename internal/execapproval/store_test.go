package execapproval

import (
	"context"
	"fmt"
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
	testSingleUse(t, store.PlanApprovals())
	testRelease(t, store)
	testRelease(t, store.PlanApprovals())

	project := fmt.Sprintf("exec-approval-prune-%d", time.Now().UnixNano())
	old := MakeKey(project, "expired-plan")
	if _, err := store.pool.Exec(ctx, `INSERT INTO assistant_exec_approvals
		(project, plan_hash, consumed_at) VALUES ($1, $2, now() - interval '2 days')`,
		project, old.Plan[:]); err != nil {
		t.Fatal(err)
	}

	// testSingleUse already pruned, so within the interval Consume leaves the
	// expired row alone.
	if used, err := store.Consume(ctx, project, "within-interval"); err != nil || !used {
		t.Fatalf("consume within the interval: used=%v err=%v", used, err)
	}
	if countRows(t, store, "assistant_exec_approvals", project, "expired-plan") != 1 {
		t.Fatal("Consume pruned again within the prune interval")
	}

	// Once the interval has passed, the next Consume prunes.
	clock := time.Now().Add(pruneInterval)
	store.prune.now = func() time.Time { return clock }
	if used, err := store.Consume(ctx, project, "new-plan"); err != nil || !used {
		t.Fatalf("consume after pruning: used=%v err=%v", used, err)
	}
	if countRows(t, store, "assistant_exec_approvals", project, "expired-plan") != 0 {
		t.Fatal("expired approval row was not pruned")
	}
}

// TestPostgresStoreLedgersPruneSeparately pins each ledger's retention: exec
// approvals go after a day and plan approvals after thirty, and neither
// ledger's prune touches the other's table. Replicas on the previous release
// prune assistant_exec_approvals after a day, so plan approvals must not live
// there.
func TestPostgresStoreLedgersPruneSeparately(t *testing.T) {
	url := testdb.URL(t)
	ctx := context.Background()
	exec, err := NewPostgresStore(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	plan := exec.PlanApprovals()
	project := fmt.Sprintf("approval-ledgers-%d", time.Now().UnixNano())

	backdate := func(table, token, age string) {
		t.Helper()
		key := MakeKey(project, token)
		if _, err := exec.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (project, plan_hash, consumed_at)
			VALUES ($1, $2, now() - $3::interval)`, table), project, key.Plan[:], age); err != nil {
			t.Fatal(err)
		}
	}
	backdate("assistant_exec_approvals", "exec-2-days", "2 days")
	backdate("assistant_plan_approvals", "plan-2-days", "2 days")
	backdate("assistant_plan_approvals", "plan-31-days", "31 days")

	// The exec prune deletes its own day-old row and no plan approval.
	if _, err := exec.Consume(ctx, project, "exec-trigger"); err != nil {
		t.Fatal(err)
	}
	if countRows(t, exec, "assistant_exec_approvals", project, "exec-2-days") != 0 {
		t.Fatal("exec prune kept a two-day-old exec approval")
	}
	if countRows(t, exec, "assistant_plan_approvals", project, "plan-31-days") != 1 {
		t.Fatal("exec prune reached the plan approvals table")
	}

	// The plan prune keeps a two-day-old approval and deletes one past thirty
	// days, so a spent plan approval refuses a replay for a month.
	if _, err := plan.Consume(ctx, project, "plan-trigger"); err != nil {
		t.Fatal(err)
	}
	if countRows(t, exec, "assistant_plan_approvals", project, "plan-2-days") != 1 {
		t.Fatal("plan prune deleted a two-day-old plan approval")
	}
	if countRows(t, exec, "assistant_plan_approvals", project, "plan-31-days") != 0 {
		t.Fatal("plan prune kept an approval past thirty days")
	}
	if used, err := plan.Consume(ctx, project, "plan-2-days"); err != nil || used {
		t.Fatalf("replay of a retained plan approval: used=%v err=%v", used, err)
	}

	// Opening again finds both tables and changes nothing.
	for range 2 {
		reopened, err := NewPostgresStore(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		reopened.Close()
	}
	if countRows(t, exec, "assistant_plan_approvals", project, "plan-2-days") != 1 {
		t.Fatal("reopening the store lost a plan approval")
	}
}

// TestPostgresStoreReleaseRacesConsume runs Release against Consume on one
// spent approval. Whichever lands first, the result must agree with the
// ledger: a fresh Consume leaves the row, a refused one is followed by the
// release. One release then lets exactly one of several Consumes through.
func TestPostgresStoreReleaseRacesConsume(t *testing.T) {
	url := testdb.URL(t)
	ctx := context.Background()
	exec, err := NewPostgresStore(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	store := exec.PlanApprovals()
	project := fmt.Sprintf("approval-release-race-%d", time.Now().UnixNano())

	for i := range 20 {
		token := fmt.Sprintf("plan-%d", i)
		if used, err := store.Consume(ctx, project, token); err != nil || !used {
			t.Fatalf("first consume: used=%v err=%v", used, err)
		}
		var wg sync.WaitGroup
		var fresh bool
		var consumeErr, releaseErr error
		wg.Add(2)
		go func() { defer wg.Done(); fresh, consumeErr = store.Consume(ctx, project, token) }()
		go func() { defer wg.Done(); releaseErr = store.Release(ctx, project, token) }()
		wg.Wait()
		if consumeErr != nil || releaseErr != nil {
			t.Fatalf("consume err=%v release err=%v", consumeErr, releaseErr)
		}
		rows := countRows(t, exec, "assistant_plan_approvals", project, token)
		if fresh && rows != 1 || !fresh && rows != 0 {
			t.Fatalf("round %d: Consume fresh=%v but %d rows remain", i, fresh, rows)
		}
	}

	if used, err := store.Consume(ctx, project, "released-once"); err != nil || !used {
		t.Fatalf("consume: used=%v err=%v", used, err)
	}
	if err := store.Release(ctx, project, "released-once"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			used, err := store.Consume(ctx, project, "released-once")
			if err != nil {
				t.Errorf("consume: %v", err)
				return
			}
			if used {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("fresh consumes after one release = %d, want exactly one", successes)
	}
}

// countRows counts the rows one approval left in table.
func countRows(t *testing.T, store *PostgresStore, table, project, token string) int {
	t.Helper()
	key := MakeKey(project, token)
	var count int
	if err := store.pool.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s
		WHERE project = $1 AND plan_hash = $2`, table), key.Project, key.Plan[:]).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
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
