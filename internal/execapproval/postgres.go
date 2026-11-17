package execapproval

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `CREATE TABLE IF NOT EXISTS assistant_exec_approvals (
  project text NOT NULL,
  plan_hash bytea NOT NULL,
  consumed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project, plan_hash)
)`

// retainColumn marks approvals the daily prune must keep. Plan approvals are
// keyed on the conversation and the plan's approval code, with no expiry of
// their own, so forgetting one would let a replay apply again. Older tables
// gain the column on boot.
const retainColumn = `ALTER TABLE assistant_exec_approvals
  ADD COLUMN IF NOT EXISTS retain boolean NOT NULL DEFAULT false`

// prunableIndex covers only the rows the prune may delete. Retained rows are
// never deleted, so an index over every row would grow with them and the
// prune would scan them all.
const prunableIndex = `CREATE INDEX IF NOT EXISTS assistant_exec_approvals_prunable_idx
  ON assistant_exec_approvals (consumed_at) WHERE NOT retain`

// dropConsumedAtIndex removes the full index prunableIndex replaces.
const dropConsumedAtIndex = `DROP INDEX IF EXISTS assistant_exec_approvals_consumed_at_idx`

// pruneInterval is how often one process deletes old exec approvals. Rows are
// kept a day, so pruning a few minutes late costs nothing, and Consume no
// longer pays for a DELETE on every call.
const pruneInterval = 10 * time.Minute

type PostgresStore struct {
	pool *pgxpool.Pool
	// retain keeps what this store consumes out of the daily prune.
	retain bool
	// prune is shared by every store over the same pool.
	prune *pruneSchedule
}

// pruneSchedule lets one prune through per pruneInterval.
type pruneSchedule struct {
	mu   sync.Mutex
	next time.Time
	now  func() time.Time
}

// due reports whether a prune should run now, and if so books the next one.
func (p *pruneSchedule) due() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if now.Before(p.next) {
		return false
	}
	p.next = now.Add(pruneInterval)
	return true
}

// retry lets the next Consume prune again, after a failed prune.
func (p *pruneSchedule) retry() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next = time.Time{}
}

func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("exec approval store: connect: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("exec approval store: schema: %w", err)
	}
	if _, err := pool.Exec(ctx, retainColumn); err != nil {
		pool.Close()
		return nil, fmt.Errorf("exec approval store: retain column: %w", err)
	}
	if _, err := pool.Exec(ctx, prunableIndex); err != nil {
		pool.Close()
		return nil, fmt.Errorf("exec approval store: index: %w", err)
	}
	if _, err := pool.Exec(ctx, dropConsumedAtIndex); err != nil {
		pool.Close()
		return nil, fmt.Errorf("exec approval store: drop old index: %w", err)
	}
	return &PostgresStore{pool: pool, prune: &pruneSchedule{now: time.Now}}, nil
}

func (s *PostgresStore) Close() { s.pool.Close() }

// Retaining returns a store over the same ledger whose approvals are never
// pruned, so a spent approval stays spent at any age. Plan approvals use it;
// exec approvals expire with their plans and keep the default. Closing either
// store closes the shared pool.
func (s *PostgresStore) Retaining() *PostgresStore {
	return &PostgresStore{pool: s.pool, retain: true, prune: s.prune}
}

func (s *PostgresStore) Consume(ctx context.Context, project, planToken string) (bool, error) {
	key := MakeKey(project, planToken)
	if err := s.pruneIfDue(ctx); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO assistant_exec_approvals (project, plan_hash, retain)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, key.Project, key.Plan[:], s.retain)
	if err != nil {
		return false, fmt.Errorf("consume exec approval: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// pruneIfDue deletes old exec approvals, at most once per pruneInterval.
// Compute plans expire within minutes. Keep a day of replay history, then
// delete old hashes so the ledger cannot grow without bound. Retained
// approvals are kept: see [PostgresStore.Retaining].
func (s *PostgresStore) pruneIfDue(ctx context.Context) error {
	if !s.prune.due() {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM assistant_exec_approvals
		WHERE consumed_at < now() - interval '1 day' AND NOT retain`); err != nil {
		s.prune.retry()
		return fmt.Errorf("prune exec approvals: %w", err)
	}
	return nil
}

func (s *PostgresStore) Release(ctx context.Context, project, planToken string) error {
	key := MakeKey(project, planToken)
	if _, err := s.pool.Exec(ctx, `DELETE FROM assistant_exec_approvals
		WHERE project = $1 AND plan_hash = $2`, key.Project, key.Plan[:]); err != nil {
		return fmt.Errorf("release exec approval: %w", err)
	}
	return nil
}
