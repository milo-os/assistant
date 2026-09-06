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
	// Kind and evidence were added after the table shipped, so they arrive as
	// ADD COLUMN IF NOT EXISTS with defaults rather than a new table: this
	// runs against a shared database on every open, and every row written
	// before kinds existed must keep reading back as MissingCapability.
	// NOT NULL DEFAULT is metadata-only on PostgreSQL 11+ — no table rewrite.
	`ALTER TABLE capability_gap_report
		ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'MissingCapability'`,
	`ALTER TABLE capability_gap_report
		ADD COLUMN IF NOT EXISTS evidence_tool text NOT NULL DEFAULT ''`,
	`ALTER TABLE capability_gap_report
		ADD COLUMN IF NOT EXISTS evidence_observed text NOT NULL DEFAULT ''`,
	`ALTER TABLE capability_gap_report
		ADD COLUMN IF NOT EXISTS evidence_contradicted_by text NOT NULL DEFAULT ''`,
	// The de-duplication key, added the same additive way and for the same
	// reason: this migrates a shared database that already holds real rows on
	// every process start. Existing rows take the '' default and keep reading
	// back exactly as before — they simply do not group (see [Store].Aggregate).
	`ALTER TABLE capability_gap_report
		ADD COLUMN IF NOT EXISTS capability_key text NOT NULL DEFAULT ''`,
	// Serves both aggregate reads: the provider-wide grouping and the
	// per-service key lookup Compose does on every turn.
	`CREATE INDEX IF NOT EXISTS capability_gap_report_provider_service_key_idx
		ON capability_gap_report (provider_project, service_name, capability_key)`,
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
		`SELECT id, provider_project, service_name, consumer_project, context_id, capability_key, capability, summary,
		        kind, evidence_tool, evidence_observed, evidence_contradicted_by, created_at
		 FROM capability_gap_report WHERE provider_project = $1 ORDER BY created_at DESC`,
		providerProject)
	if err != nil {
		return nil, fmt.Errorf("gapreport store: list: %w", err)
	}
	defer rows.Close()

	var out []Report
	for rows.Next() {
		var r Report
		var kind string
		if err := rows.Scan(&r.ID, &r.ProviderProject, &r.ServiceName, &r.ConsumerProject,
			&r.ContextID, &r.CapabilityKey, &r.Capability, &r.Summary, &kind,
			&r.Evidence.Tool, &r.Evidence.Observed, &r.Evidence.ContradictedBy, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("gapreport store: scan report: %w", err)
		}
		// A row written before the kind column existed carries the column
		// default; read anything empty as the documented default rather than
		// surfacing "" to callers.
		r.Kind = Kind(kind)
		if r.Kind == "" {
			r.Kind = KindMissingCapability
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
func (s *PostgresStore) Insert(ctx context.Context, params InsertParams) (Report, error) {
	p, err := normalize(params)
	if err != nil {
		return Report{}, err
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
		p.ProviderProject).Scan(&count); err != nil {
		return Report{}, fmt.Errorf("gapreport store: count reports: %w", err)
	}
	if count >= MaxReportsPerProject {
		return Report{}, ErrProjectFull
	}

	r := Report{
		ID:              newReportID(),
		ProviderProject: p.ProviderProject,
		ServiceName:     p.ServiceName,
		ConsumerProject: p.ConsumerProject,
		ContextID:       p.ContextID,
		CapabilityKey:   p.CapabilityKey,
		Capability:      p.Capability,
		Summary:         p.Summary,
		Kind:            p.Kind,
		Evidence:        p.Evidence,
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO capability_gap_report
		   (id, provider_project, service_name, consumer_project, context_id, capability_key, capability, summary,
		    kind, evidence_tool, evidence_observed, evidence_contradicted_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 RETURNING created_at`,
		r.ID, r.ProviderProject, r.ServiceName, r.ConsumerProject, r.ContextID, r.CapabilityKey, r.Capability, r.Summary,
		string(r.Kind), r.Evidence.Tool, r.Evidence.Observed, r.Evidence.ContradictedBy,
	).Scan(&r.CreatedAt); err != nil {
		return Report{}, fmt.Errorf("gapreport store: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("gapreport store: commit insert: %w", err)
	}
	return r, nil
}

// aggregateQuery groups a provider project's reports into distinct gaps. The
// grouping key is the capability key, or the report's own id when it has none
// — a keyless row becomes a group of one rather than merging with every other
// keyless row (see [Store].Aggregate). Capability and kind come from the most
// recent occurrence via array_agg ... ORDER BY, and Postgres computes
// count(DISTINCT context_id) directly, so "how many conversations" never has
// to be assembled client-side.
const aggregateQuery = `
	SELECT grp,
	       max(capability_key)                             AS capability_key,
	       service_name,
	       (array_agg(capability ORDER BY created_at DESC))[1] AS capability,
	       (array_agg(kind       ORDER BY created_at DESC))[1] AS kind,
	       count(DISTINCT context_id)                      AS conversations,
	       count(*)                                        AS occurrences,
	       min(created_at)                                 AS first_seen,
	       max(created_at)                                 AS last_seen
	FROM (
		SELECT *,
		       CASE WHEN capability_key = '' THEN id ELSE capability_key END AS grp
		FROM capability_gap_report
		WHERE provider_project = $1
	) t
	GROUP BY grp, service_name
	ORDER BY conversations DESC, last_seen DESC, grp ASC`

// Aggregate implements [Store].
func (s *PostgresStore) Aggregate(ctx context.Context, providerProject string) ([]Aggregate, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, aggregateQuery, providerProject)
	if err != nil {
		return nil, fmt.Errorf("gapreport store: aggregate: %w", err)
	}
	defer rows.Close()

	var out []Aggregate
	for rows.Next() {
		var a Aggregate
		var kind string
		if err := rows.Scan(&a.Key, &a.CapabilityKey, &a.ServiceName, &a.Capability, &kind,
			&a.Conversations, &a.Occurrences, &a.FirstSeen, &a.LastSeen); err != nil {
			return nil, fmt.Errorf("gapreport store: scan aggregate: %w", err)
		}
		// Same default as List: a row written before kinds existed carries
		// the column default and means MissingCapability.
		a.Kind = Kind(kind)
		if a.Kind == "" {
			a.Kind = KindMissingCapability
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gapreport store: aggregate: %w", err)
	}
	return out, nil
}

// CapabilityKeys implements [Store]. Most-hit first, so a cap can only ever
// drop the keys least likely to be re-filed.
func (s *PostgresStore) CapabilityKeys(ctx context.Context, providerProject, serviceName string, limit int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	// limit <= 0 means unbounded; NULL is Postgres's own way to say that.
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx,
		`SELECT capability_key
		 FROM capability_gap_report
		 WHERE provider_project = $1 AND service_name = $2 AND capability_key <> ''
		 GROUP BY capability_key
		 ORDER BY count(DISTINCT context_id) DESC, max(created_at) DESC, capability_key ASC
		 LIMIT $3`,
		providerProject, serviceName, lim)
	if err != nil {
		return nil, fmt.Errorf("gapreport store: capability keys: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("gapreport store: scan capability key: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gapreport store: capability keys: %w", err)
	}
	return out, nil
}
