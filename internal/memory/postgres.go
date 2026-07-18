package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// dbOpTimeout bounds every database method internally, mirroring
	// internal/history's rationale: a wedged-but-TCP-alive backend errors out
	// instead of blocking forever.
	dbOpTimeout = 15 * time.Second
	// poolMaxConns caps the connection pool.
	poolMaxConns = 4
	// statementTimeout is the server-side ceiling on any single statement.
	statementTimeout = "10000"
)

// schema is the storage layer for project memory. Statements are idempotent
// (IF NOT EXISTS) — migrate-on-open, no versioning machinery, matching
// internal/history/postgres.go's convention (this repo has no migration
// framework).
var schema = []string{
	`CREATE TABLE IF NOT EXISTS project_memory (
		project_name text        NOT NULL,
		key          text        NOT NULL,
		value        text        NOT NULL,
		updated_at   timestamptz NOT NULL DEFAULT now(),
		created_at   timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project_name, key)
	)`,
}

// PostgresStore is a durable [Store] on PostgreSQL. Safe for concurrent use.
// Construct with [NewPostgresStore], release with Close.
type PostgresStore struct {
	pool *pgxpool.Pool
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore connects to databaseURL (a postgres:// URL), verifies the
// connection, and applies the schema. It fails fast on an unreachable or
// unwilling database.
func NewPostgresStore(ctx context.Context, databaseURL string, logger *slog.Logger) (*PostgresStore, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	cfg, err := buildPoolConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("memory store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("memory store: ping: %w", err)
	}
	for _, stmt := range schema {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			pool.Close()
			return nil, fmt.Errorf("memory store: apply schema: %w", err)
		}
	}
	logger.Info("memory.store", "type", "postgres", "host", cfg.ConnConfig.Host, "database", cfg.ConnConfig.Database)
	return &PostgresStore{pool: pool}, nil
}

func buildPoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("memory store: parse database url: %w", err)
	}
	cfg.MaxConns = poolMaxConns
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = statementTimeout
	}
	return cfg, nil
}

// Close releases the connection pool.
func (s *PostgresStore) Close() { s.pool.Close() }

// List implements [Store].
func (s *PostgresStore) List(ctx context.Context, projectName string) ([]Fact, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT key, value, updated_at FROM project_memory WHERE project_name = $1 ORDER BY key`,
		projectName)
	if err != nil {
		return nil, fmt.Errorf("memory store: list: %w", err)
	}
	defer rows.Close()

	var out []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.Key, &f.Value, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("memory store: scan fact: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory store: list: %w", err)
	}
	return out, nil
}

// Get implements [Store].
func (s *PostgresStore) Get(ctx context.Context, projectName, key string) (Fact, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	var f Fact
	err := s.pool.QueryRow(ctx,
		`SELECT key, value, updated_at FROM project_memory WHERE project_name = $1 AND key = $2`,
		projectName, key).Scan(&f.Key, &f.Value, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Fact{}, false, nil
	}
	if err != nil {
		return Fact{}, false, fmt.Errorf("memory store: get: %w", err)
	}
	return f, true, nil
}

// Upsert implements [Store]. The project fact-count bound is enforced inside
// the same transaction as the write, so concurrent inserts cannot race past
// MaxFactsPerProject.
func (s *PostgresStore) Upsert(ctx context.Context, projectName, key, value string) error {
	if len(value) > MaxFactValueLen {
		return ErrValueTooLong
	}
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("memory store: begin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM project_memory WHERE project_name = $1 AND key = $2)`,
		projectName, key).Scan(&exists); err != nil {
		return fmt.Errorf("memory store: check existing: %w", err)
	}
	if !exists {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM project_memory WHERE project_name = $1`,
			projectName).Scan(&count); err != nil {
			return fmt.Errorf("memory store: count facts: %w", err)
		}
		if count >= MaxFactsPerProject {
			return ErrProjectFull
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO project_memory (project_name, key, value)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (project_name, key) DO UPDATE
		   SET value = EXCLUDED.value, updated_at = now()`,
		projectName, key, value); err != nil {
		return fmt.Errorf("memory store: upsert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("memory store: commit upsert: %w", err)
	}
	return nil
}

// Delete implements [Store].
func (s *PostgresStore) Delete(ctx context.Context, projectName, key string) error {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM project_memory WHERE project_name = $1 AND key = $2`,
		projectName, key); err != nil {
		return fmt.Errorf("memory store: delete: %w", err)
	}
	return nil
}
