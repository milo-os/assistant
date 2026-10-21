package execapproval

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `CREATE TABLE IF NOT EXISTS assistant_exec_approvals (
  project text NOT NULL,
  requester_hash bytea NOT NULL,
  plan_hash bytea NOT NULL,
  consumed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project, requester_hash, plan_hash)
)`

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
	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) Close() { s.pool.Close() }

func (s *PostgresStore) Consume(ctx context.Context, project, requesterToken, planToken string) (bool, error) {
	key := MakeKey(project, requesterToken, planToken)
	tag, err := s.pool.Exec(ctx, `INSERT INTO assistant_exec_approvals (project, requester_hash, plan_hash)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, key.Project, key.Requester[:], key.Plan[:])
	if err != nil {
		return false, fmt.Errorf("consume exec approval: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
