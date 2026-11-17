package execapproval

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `CREATE TABLE IF NOT EXISTS assistant_exec_approvals (
  project text NOT NULL,
  plan_hash bytea NOT NULL,
  consumed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project, plan_hash)
)`

const permanentColumn = `ALTER TABLE assistant_exec_approvals
  ADD COLUMN IF NOT EXISTS permanent boolean NOT NULL DEFAULT false`

const consumedAtIndex = `CREATE INDEX IF NOT EXISTS assistant_exec_approvals_consumed_at_idx
  ON assistant_exec_approvals (consumed_at)`

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("exec approval store: connect: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("exec approval store: schema: %w", err)
	}
	if _, err := pool.Exec(ctx, permanentColumn); err != nil {
		pool.Close()
		return nil, fmt.Errorf("exec approval store: permanent column: %w", err)
	}
	if _, err := pool.Exec(ctx, consumedAtIndex); err != nil {
		pool.Close()
		return nil, fmt.Errorf("exec approval store: index: %w", err)
	}
	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) Close() { s.pool.Close() }

func (s *PostgresStore) Consume(ctx context.Context, project, planToken string) (bool, error) {
	return s.consume(ctx, project, planToken, false)
}

func (s *PostgresStore) ConsumePermanently(ctx context.Context, project, key string) (bool, error) {
	return s.consume(ctx, project, key, true)
}

func (s *PostgresStore) consume(ctx context.Context, project, planToken string, permanent bool) (bool, error) {
	key := MakeKey(project, planToken)
	// Compute plans and plan tokens expire within minutes. Keep a day of
	// replay history for them, then delete old hashes so the ledger cannot
	// grow without bound. Permanent records are never pruned.
	if _, err := s.pool.Exec(ctx, `DELETE FROM assistant_exec_approvals
		WHERE NOT permanent AND consumed_at < now() - interval '1 day'`); err != nil {
		return false, fmt.Errorf("prune exec approvals: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO assistant_exec_approvals (project, plan_hash, permanent)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, key.Project, key.Plan[:], permanent)
	if err != nil {
		return false, fmt.Errorf("consume exec approval: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
