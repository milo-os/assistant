package execapproval

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestMemoryStoreSingleUseAcrossConcurrentCalls(t *testing.T) {
	testSingleUse(t, NewMemoryStore())
}

func TestPostgresStoreSingleUseAndPruning(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
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
	if used, err := store.Consume(ctx, project, "new-plan"); err != nil || !used {
		t.Fatalf("consume after pruning: used=%v err=%v", used, err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM assistant_exec_approvals
		WHERE project = $1 AND plan_hash = $2`, project, old.Plan[:]).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("expired approval row was not pruned")
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
