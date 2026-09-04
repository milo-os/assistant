// Package taskstore provides a durable, tenant-aware [taskstore.Store]
// implementation for the A2A task lifecycle. The a2a-go server stack keeps
// tasks in an in-memory store by default (lost on restart, no per-tenant
// filter); this package backs them on the SAME PostgreSQL database as the
// conversation history (CONVERSATION_STORE_URL) so tasks survive restarts and
// carry the owning Milo project on every row.
//
// The exported store implements the a2a-go
// [github.com/a2aproject/a2a-go/v2/a2asrv/taskstore].Store interface (imported
// here as a2astore) — Create/Update/Get/List with the StoredTask / UpdateRequest
// / TaskVersion optimistic-concurrency contract. It ADDS a project-scoped
// listing primitive ([PostgresStore.ListForProjects]) that the interface's
// project-blind List cannot express, so a future tenant-scoped list endpoint has
// a tenant-safe query to build on.
package taskstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2astore "github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	assistanta2a "github.com/milo-os/assistant/internal/a2a"
)

const (
	// dbOpTimeout bounds every database method internally, mirroring
	// internal/history/postgres.go: each method wraps its incoming context with
	// this deadline before touching the pool so a wedged-but-TCP-alive backend
	// errors out instead of blocking forever.
	dbOpTimeout = 15 * time.Second

	// poolMaxConns caps the connection pool so a stalled backend can wedge at
	// most this many acquirers; further Acquire calls fail fast on the (always
	// bounded) per-method context. Matches the conversation store.
	poolMaxConns = 8

	// statementTimeout is the server-side ceiling on any single statement, sent
	// as a connection RuntimeParam (milliseconds). Fires before dbOpTimeout for
	// the common slow-query case. Matches the conversation store.
	statementTimeout = "10000"

	// defaultPageSize / maxPageSize mirror the a2a-go in-memory store's List
	// bounds so a caller sees identical paging semantics on either backend.
	defaultPageSize = 50
	maxPageSize     = 100
)

// schema is the durable task table. Tasks are stored as JSONB keyed by task ID
// with a monotonic version column for optimistic concurrency (the TaskVersion
// contract) and the owning project denormalized onto its own column so listing
// can be scoped by tenant without unmarshaling every row. Statements are
// idempotent (IF NOT EXISTS) — migrate-on-open, same posture as the history
// store.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS a2a_tasks (
		task_id      text        PRIMARY KEY,
		project_name text        NOT NULL DEFAULT '',
		context_id   text        NOT NULL DEFAULT '',
		state        text        NOT NULL DEFAULT '',
		version      bigint      NOT NULL,
		task         jsonb       NOT NULL,
		updated_at   timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS a2a_tasks_by_project_updated
		ON a2a_tasks (project_name, updated_at DESC, task_id DESC)`,
}

// PostgresStore is a durable, tenant-aware [a2astore.Store] on PostgreSQL. Safe
// for concurrent use; updates to the same task serialize on its row via
// SELECT ... FOR UPDATE. Construct with [NewPostgresStore], release with Close.
type PostgresStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ a2astore.Store = (*PostgresStore)(nil)

// NewPostgresStore connects to databaseURL (a postgres:// URL — the same
// CONVERSATION_STORE_URL the history store uses), verifies the connection, and
// applies the schema. It fails fast on an unreachable database: a service
// configured for durable tasks must not silently fall back to amnesia.
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
		return nil, fmt.Errorf("task store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("task store: ping: %w", err)
	}
	for _, stmt := range schema {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			pool.Close()
			return nil, fmt.Errorf("task store: apply schema: %w", err)
		}
	}
	logger.Info("task.store", "type", "postgres", "host", cfg.ConnConfig.Host, "database", cfg.ConnConfig.Database)
	return &PostgresStore{pool: pool, now: time.Now}, nil
}

// buildPoolConfig parses databaseURL and layers on the same pool/backend bounds
// as the conversation store: a capped MaxConns and a server-side
// statement_timeout. Operator-supplied values in the URL win. Exposed at package
// scope so it can be asserted offline (no live database).
func buildPoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("task store: parse database url: %w", err)
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

// Ping verifies the database is reachable. It backs the server's readiness probe
// (GET /readyz) so traffic is withheld until durable storage is available.
func (s *PostgresStore) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("task store: ping: %w", err)
	}
	return nil
}

// Create implements [a2astore.Store]: it inserts a new task at version 1,
// denormalizing the owning project and context onto their columns for scoped
// listing. Returns [a2astore.ErrTaskAlreadyExists] when the ID already exists.
func (s *PostgresStore) Create(ctx context.Context, task *a2a.Task) (a2astore.TaskVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()

	data, err := json.Marshal(task)
	if err != nil {
		return a2astore.TaskVersionMissing, fmt.Errorf("task store: marshal task: %w", err)
	}
	const version = a2astore.TaskVersion(1)
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO a2a_tasks (task_id, project_name, context_id, state, version, task, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (task_id) DO NOTHING`,
		string(task.ID), projectOf(task), task.ContextID, string(task.Status.State),
		int64(version), data, s.now())
	if err != nil {
		return a2astore.TaskVersionMissing, fmt.Errorf("task store: create: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return a2astore.TaskVersionMissing, a2astore.ErrTaskAlreadyExists
	}
	return version, nil
}

// Update implements [a2astore.Store]: it replaces the stored task under
// optimistic concurrency. The current row is locked (SELECT ... FOR UPDATE) so
// concurrent updates serialize; a non-missing PrevVersion that no longer matches
// the stored version is rejected with [a2astore.ErrConcurrentModification], and
// an absent task with [a2a.ErrTaskNotFound].
func (s *PostgresStore) Update(ctx context.Context, req *a2astore.UpdateRequest) (a2astore.TaskVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()

	data, err := json.Marshal(req.Task)
	if err != nil {
		return a2astore.TaskVersionMissing, fmt.Errorf("task store: marshal task: %w", err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return a2astore.TaskVersionMissing, fmt.Errorf("task store: begin update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		curVersion int64
		curProject string
	)
	err = tx.QueryRow(ctx,
		`SELECT version, project_name FROM a2a_tasks WHERE task_id = $1 FOR UPDATE`,
		string(req.Task.ID)).Scan(&curVersion, &curProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return a2astore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if err != nil {
		return a2astore.TaskVersionMissing, fmt.Errorf("task store: lock task: %w", err)
	}
	if req.PrevVersion != a2astore.TaskVersionMissing && a2astore.TaskVersion(curVersion) != req.PrevVersion {
		return a2astore.TaskVersionMissing, a2astore.ErrConcurrentModification
	}

	// The owning project is set once at Create and is immutable: never let an
	// update blank it out (a later turn's task may carry no projectName), so the
	// authorization key on the row stays stable across the task's lifetime.
	project := projectOf(req.Task)
	if project == "" {
		project = curProject
	}

	newVersion := a2astore.TaskVersion(curVersion + 1)
	if _, err := tx.Exec(ctx,
		`UPDATE a2a_tasks
		 SET project_name = $1, context_id = $2, state = $3, version = $4, task = $5, updated_at = $6
		 WHERE task_id = $7`,
		project, req.Task.ContextID, string(req.Task.Status.State), int64(newVersion), data, s.now(),
		string(req.Task.ID)); err != nil {
		return a2astore.TaskVersionMissing, fmt.Errorf("task store: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return a2astore.TaskVersionMissing, fmt.Errorf("task store: commit update: %w", err)
	}
	return newVersion, nil
}

// Get implements [a2astore.Store]: it returns the stored task and its version,
// or [a2a.ErrTaskNotFound].
func (s *PostgresStore) Get(ctx context.Context, taskID a2a.TaskID) (*a2astore.StoredTask, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()

	var (
		data    []byte
		version int64
	)
	err := s.pool.QueryRow(ctx,
		`SELECT task, version FROM a2a_tasks WHERE task_id = $1`, string(taskID)).Scan(&data, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("task store: get: %w", err)
	}
	var task a2a.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("task store: unmarshal task: %w", err)
	}
	return &a2astore.StoredTask{Task: &task, Version: a2astore.TaskVersion(version)}, nil
}

// List implements [a2astore.Store]. The A2A ListTasks RPC carries no caller
// identity in its request, so this method CANNOT scope the result to the caller
// on its own — the server middleware therefore denies the ListTasks RPC outright
// (internal/server/middleware.go). To keep the store tenant-safe even if it is
// ever reached, List returns only tasks whose project is in the scope carried on
// the context ([WithProjectScope]); with no scope on the context it returns an
// empty page rather than leaking cross-tenant tasks. The tenant-safe primitive a
// future scoped endpoint should call is [PostgresStore.ListForProjects].
func (s *PostgresStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	scope, all := projectScopeFromContext(ctx)
	return s.list(ctx, scope, all, req)
}

// ListForProjects lists tasks owned by any of the granted projects, newest
// activity first, with the same paging/filter semantics as [List]. It is the
// tenant-safe listing primitive: pass the caller's granted project set. An empty
// projects slice returns an empty page (no grants ⇒ no tasks).
func (s *PostgresStore) ListForProjects(ctx context.Context, projects []string, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return s.list(ctx, projects, false, req)
}

func (s *PostgresStore) list(ctx context.Context, projects []string, allProjects bool, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()

	if req == nil {
		req = &a2a.ListTasksRequest{}
	}
	pageSize := req.PageSize
	switch {
	case pageSize == 0:
		pageSize = defaultPageSize
	case pageSize < 1 || pageSize > maxPageSize:
		return nil, fmt.Errorf("task store: page size must be between 1 and %d inclusive, got %d: %w", maxPageSize, pageSize, a2a.ErrInvalidRequest)
	}

	// No scope and not "all" ⇒ nothing is visible. Return an empty page rather
	// than every tenant's tasks.
	if !allProjects && len(projects) == 0 {
		return &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, PageSize: pageSize}, nil
	}

	where, args := s.buildFilter(projects, allProjects, req)

	// TotalSize is the count before pagination, matching the in-memory store.
	var totalSize int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM a2a_tasks `+where, args...).Scan(&totalSize); err != nil {
		return nil, fmt.Errorf("task store: count: %w", err)
	}

	// Keyset pagination on (updated_at DESC, task_id DESC). Fetch one extra row
	// to decide whether a next page exists.
	pageArgs := args
	pageWhere := where
	if req.PageToken != "" {
		cursorTime, cursorID, err := decodePageToken(req.PageToken)
		if err != nil {
			return nil, err
		}
		pageWhere += fmt.Sprintf(" AND (updated_at, task_id) < ($%d, $%d)", len(pageArgs)+1, len(pageArgs)+2)
		pageArgs = append(pageArgs, cursorTime, cursorID)
	}
	pageArgs = append(pageArgs, pageSize+1)
	rows, err := s.pool.Query(ctx,
		`SELECT task, updated_at, task_id FROM a2a_tasks `+pageWhere+
			fmt.Sprintf(` ORDER BY updated_at DESC, task_id DESC LIMIT $%d`, len(pageArgs)), pageArgs...)
	if err != nil {
		return nil, fmt.Errorf("task store: list: %w", err)
	}
	defer rows.Close()

	type row struct {
		task      *a2a.Task
		updatedAt time.Time
		id        a2a.TaskID
	}
	var fetched []row
	for rows.Next() {
		var (
			data      []byte
			updatedAt time.Time
			id        string
		)
		if err := rows.Scan(&data, &updatedAt, &id); err != nil {
			return nil, fmt.Errorf("task store: scan task: %w", err)
		}
		var task a2a.Task
		if err := json.Unmarshal(data, &task); err != nil {
			return nil, fmt.Errorf("task store: unmarshal task: %w", err)
		}
		fetched = append(fetched, row{task: &task, updatedAt: updatedAt, id: a2a.TaskID(id)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task store: list: %w", err)
	}

	var nextPageToken string
	if len(fetched) > pageSize {
		last := fetched[pageSize-1]
		nextPageToken = encodePageToken(last.updatedAt, last.id)
		fetched = fetched[:pageSize]
	}

	tasks := make([]*a2a.Task, 0, len(fetched))
	for _, r := range fetched {
		tasks = append(tasks, trimForList(r.task, req))
	}
	return &a2a.ListTasksResponse{
		Tasks:         tasks,
		TotalSize:     totalSize,
		PageSize:      pageSize,
		NextPageToken: nextPageToken,
	}, nil
}

// buildFilter assembles the WHERE clause and positional args shared by the count
// and page queries. Project scoping comes first so it is never optional: unless
// allProjects is set, the query is constrained to the granted project set.
func (s *PostgresStore) buildFilter(projects []string, allProjects bool, req *a2a.ListTasksRequest) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	if !allProjects {
		args = append(args, projects)
		clauses = append(clauses, fmt.Sprintf("project_name = ANY($%d)", len(args)))
	}
	if req.ContextID != "" {
		args = append(args, req.ContextID)
		clauses = append(clauses, fmt.Sprintf("context_id = $%d", len(args)))
	}
	if req.Status != a2a.TaskStateUnspecified {
		args = append(args, string(req.Status))
		clauses = append(clauses, fmt.Sprintf("state = $%d", len(args)))
	}
	if req.StatusTimestampAfter != nil {
		args = append(args, *req.StatusTimestampAfter)
		clauses = append(clauses, fmt.Sprintf("updated_at >= $%d", len(args)))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

// trimForList applies the request's history/artifact projection to a task copy,
// mirroring the in-memory store's toListTasksResult.
func trimForList(task *a2a.Task, req *a2a.ListTasksRequest) *a2a.Task {
	const defaultMaxHistoryLength = 100
	historyLength := defaultMaxHistoryLength
	if req.HistoryLength != nil {
		historyLength = *req.HistoryLength
	}
	if historyLength == 0 {
		task.History = []*a2a.Message{}
	} else if historyLength > 0 && len(task.History) > historyLength {
		task.History = task.History[len(task.History)-historyLength:]
	}
	if !req.IncludeArtifacts {
		task.Artifacts = nil
	}
	return task
}

// projectOf reads the owning Milo project off the task metadata, reusing the
// single-sourced projectName contract from internal/a2a.
func projectOf(task *a2a.Task) string {
	if task == nil {
		return ""
	}
	return assistanta2a.ProjectName(nil, task.Metadata)
}

// ── page token (shared wire with the a2a-go in-memory store) ──

func encodePageToken(updatedTime time.Time, taskID a2a.TaskID) string {
	return base64.URLEncoding.EncodeToString([]byte(fmt.Sprintf("%s_%s", updatedTime.Format(time.RFC3339Nano), taskID)))
}

func decodePageToken(token string) (time.Time, a2a.TaskID, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	parts := strings.SplitN(string(decoded), "_", 2)
	if len(parts) != 2 {
		return time.Time{}, "", a2a.ErrParseError
	}
	updatedTime, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	return updatedTime, a2a.TaskID(parts[1]), nil
}
