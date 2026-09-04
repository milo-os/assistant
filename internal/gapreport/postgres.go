package gapreport

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// dbOpTimeout bounds every database method internally, mirroring
	// internal/memory's rationale: a wedged-but-TCP-alive backend errors out
	// instead of blocking forever.
	dbOpTimeout = 15 * time.Second
	// poolMaxConns caps the connection pool.
	poolMaxConns = 4
	// statementTimeout is the server-side ceiling on any single statement.
	statementTimeout = "10000"
)

// schema is the storage layer for capability-gap reports. Statements are
// idempotent (IF NOT EXISTS) — migrate-on-open, no versioning machinery,
// matching internal/memory/postgres.go's convention (this repo has no
// migration framework).
var schema = []string{
	`CREATE TABLE IF NOT EXISTS capability_gap_report (
		id                text        PRIMARY KEY,
		provider_project  text        NOT NULL,
		service_name      text        NOT NULL,
		consumer_project  text        NOT NULL,
		context_id        text        NOT NULL,
		capability        text        NOT NULL,
		summary           text        NOT NULL,
		created_at        timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS capability_gap_report_provider_project_idx
		ON capability_gap_report (provider_project, created_at DESC)`,
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
		return nil, fmt.Errorf("gapreport store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("gapreport store: ping: %w", err)
	}
	for _, stmt := range schema {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			pool.Close()
			return nil, fmt.Errorf("gapreport store: apply schema: %w", err)
		}
	}
	logger.Info("gapreport.store", "type", "postgres", "host", cfg.ConnConfig.Host, "database", cfg.ConnConfig.Database)
	return &PostgresStore{pool: pool}, nil
}

func buildPoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("gapreport store: parse database url: %w", err)
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
func (s *PostgresStore) List(ctx context.Context, providerProject string) ([]Report, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT id, provider_project, service_name, consumer_project, context_id, capability, summary, created_at
		 FROM capability_gap_report WHERE provider_project = $1 ORDER BY created_at DESC`,
		providerProject)
	if err != nil {
		return nil, fmt.Errorf("gapreport store: list: %w", err)
	}
	defer rows.Close()

	var out []Report
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.ID, &r.ProviderProject, &r.ServiceName, &r.ConsumerProject,
			&r.ContextID, &r.Capability, &r.Summary, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("gapreport store: scan report: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gapreport store: list: %w", err)
	}
	return out, nil
}

// Insert implements [Store]. The project report-count bound is enforced
// inside the same transaction as the write, so concurrent inserts cannot
// race past MaxReportsPerProject.
func (s *PostgresStore) Insert(ctx context.Context, providerProject, serviceName, consumerProject, contextID, capability, summary string) (Report, error) {
	if len(capability) > MaxCapabilityLen {
		return Report{}, ErrCapabilityTooLong
	}
	if len(summary) > MaxSummaryLen {
		return Report{}, ErrSummaryTooLong
	}
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Report{}, fmt.Errorf("gapreport store: begin insert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM capability_gap_report WHERE provider_project = $1`,
		providerProject).Scan(&count); err != nil {
		return Report{}, fmt.Errorf("gapreport store: count reports: %w", err)
	}
	if count >= MaxReportsPerProject {
		return Report{}, ErrProjectFull
	}

	r := Report{
		ID:              newReportID(),
		ProviderProject: providerProject,
		ServiceName:     serviceName,
		ConsumerProject: consumerProject,
		ContextID:       contextID,
		Capability:      capability,
		Summary:         summary,
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO capability_gap_report
		   (id, provider_project, service_name, consumer_project, context_id, capability, summary)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING created_at`,
		r.ID, r.ProviderProject, r.ServiceName, r.ConsumerProject, r.ContextID, r.Capability, r.Summary,
	).Scan(&r.CreatedAt); err != nil {
		return Report{}, fmt.Errorf("gapreport store: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("gapreport store: commit insert: %w", err)
	}
	return r, nil
}
