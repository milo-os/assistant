package execapproval

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ledger names one approvals table and how long its rows are kept. Table
// names are constants here and never come from input.
type ledger struct {
	table     string
	retention string // a Postgres interval
}

// execLedger holds compute exec approvals. Compute plans expire within
// minutes, so a day of replay history is plenty.
var execLedger = ledger{table: "assistant_exec_approvals", retention: "1 day"}

// planLedger holds resources_apply plan approvals. They are keyed on the
// project and the plan's approval code, which stays the same for every
// re-plan of the same change, so a row must outlive any plan token minted for
// that change: thirty days. They have their own table because replicas still
// running the previous release prune assistant_exec_approvals after a day,
// and must never reach these rows.
var planLedger = ledger{table: "assistant_plan_approvals", retention: "30 days"}

func (l ledger) schema() string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  project text NOT NULL,
  plan_hash bytea NOT NULL,
  consumed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project, plan_hash)
)`, l.table)
}

func (l ledger) consumedAtIndex() string {
	return fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_consumed_at_idx ON %s (consumed_at)`, l.table, l.table)
}

// pruneInterval is how often one store deletes rows past its retention.
// Pruning a few minutes late costs nothing, and Consume does not pay for a
// DELETE on every call.
const pruneInterval = 10 * time.Minute

// PostgresStore is one approvals ledger shared by every assistant replica.
type PostgresStore struct {
	pool   *pgxpool.Pool
	ledger ledger
	prune  *pruneSchedule
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

// NewPostgresStore connects, creates both approval tables if they are
// missing, and returns the exec approvals ledger. [PostgresStore.PlanApprovals]
// gives the plan approvals ledger over the same pool.
func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("exec approval store: connect: %w", err)
	}
	for _, l := range []ledger{execLedger, planLedger} {
		if _, err := pool.Exec(ctx, l.schema()); err != nil {
			pool.Close()
			return nil, fmt.Errorf("exec approval store: %s schema: %w", l.table, err)
		}
		if _, err := pool.Exec(ctx, l.consumedAtIndex()); err != nil {
			pool.Close()
			return nil, fmt.Errorf("exec approval store: %s index: %w", l.table, err)
		}
	}
	return newStore(pool, execLedger), nil
}

func newStore(pool *pgxpool.Pool, l ledger) *PostgresStore {
	return &PostgresStore{pool: pool, ledger: l, prune: &pruneSchedule{now: time.Now}}
}

// PlanApprovals returns the plan approvals ledger, over the same pool.
// Closing either store closes the pool.
func (s *PostgresStore) PlanApprovals() *PostgresStore {
	return newStore(s.pool, planLedger)
}

func (s *PostgresStore) Close() { s.pool.Close() }

func (s *PostgresStore) Consume(ctx context.Context, project, planToken string) (bool, error) {
	key := MakeKey(project, planToken)
	s.pruneIfDue(ctx)
	tag, err := s.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (project, plan_hash)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, s.ledger.table), key.Project, key.Plan[:])
	if err != nil {
		return false, fmt.Errorf("consume %s: %w", s.ledger.table, err)
	}
	return tag.RowsAffected() == 1, nil
}

// pruneIfDue deletes rows past the ledger's retention, at most once per
// pruneInterval, so the ledger cannot grow without bound. Pruning is best
// effort: a failed prune is logged and retried on the next Consume, and never
// refuses the approval being recorded.
func (s *PostgresStore) pruneIfDue(ctx context.Context) {
	if !s.prune.due() {
		return
	}
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s
		WHERE consumed_at < now() - $1::interval`, s.ledger.table), s.ledger.retention); err != nil {
		s.prune.retry()
		slog.WarnContext(ctx, "execapproval.prune_failed", "table", s.ledger.table, "error", err.Error())
	}
}

func (s *PostgresStore) Release(ctx context.Context, project, planToken string) error {
	key := MakeKey(project, planToken)
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s
		WHERE project = $1 AND plan_hash = $2`, s.ledger.table), key.Project, key.Plan[:]); err != nil {
		return fmt.Errorf("release %s: %w", s.ledger.table, err)
	}
	return nil
}
