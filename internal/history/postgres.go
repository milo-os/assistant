package history

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultMaxRecentTurns bounds how many of a conversation's newest turns the
// Postgres store fetches for replay. It exists so the replay query is
// O(bounded) regardless of conversation length — the agent layer's token
// budget then truncates further. 200 turns comfortably exceeds any sane token
// budget (6000 estimated tokens ≈ 24k chars ≈ a few dozen turns).
const DefaultMaxRecentTurns = 200

// schema is the storage layer for conversations and messages. Everything is
// keyed by (project_name, context_id) — the project is the authorization
// boundary, and no query in this file ever crosses it.
//
// Messages are one row per message (not per turn) so a future conversation
// API (list/read subresource) can serve them directly. seq is the absolute
// 1-based message index within the conversation: turn k is the pair
// (seq 2k-1 = user, seq 2k = assistant), which lets replay reconstruct turns
// by arithmetic instead of trusting row adjacency.
//
// Statements are idempotent (IF NOT EXISTS) — migrate-on-open, no versioning
// machinery until the schema actually changes shape.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS conversations (
		project_name   text        NOT NULL,
		context_id     text        NOT NULL,
		created_at     timestamptz NOT NULL DEFAULT now(),
		last_active_at timestamptz NOT NULL DEFAULT now(),
		turn_count     bigint      NOT NULL DEFAULT 0,
		PRIMARY KEY (project_name, context_id)
	)`,
	`CREATE TABLE IF NOT EXISTS messages (
		project_name text        NOT NULL,
		context_id   text        NOT NULL,
		seq          bigint      NOT NULL,
		role         text        NOT NULL CHECK (role IN ('user', 'assistant')),
		content      text        NOT NULL,
		created_at   timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project_name, context_id, seq),
		FOREIGN KEY (project_name, context_id)
			REFERENCES conversations (project_name, context_id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS conversations_by_project_activity
		ON conversations (project_name, last_active_at DESC)`,
}

// Conversation is the stored metadata of one conversation, for per-project
// listing (newest activity first).
type Conversation struct {
	ProjectName  string
	ContextID    string
	CreatedAt    time.Time
	LastActiveAt time.Time
	TurnCount    int64
}

// Lister lists a project's conversations. It is a separate interface from
// [Store] because the agent loop never needs it — it exists for consumers
// (a conversation-list API) and for operational inspection.
type Lister interface {
	ListConversations(ctx context.Context, projectName string, limit int) ([]Conversation, error)
}

// PostgresStore is a durable [Store] on PostgreSQL. Safe for concurrent use;
// concurrent appends to the same conversation serialize on the conversation
// row. Construct with [NewPostgresStore], release with Close.
type PostgresStore struct {
	pool           *pgxpool.Pool
	maxRecentTurns int
}

var (
	_ Store  = (*PostgresStore)(nil)
	_ Lister = (*PostgresStore)(nil)
)

// NewPostgresStore connects to databaseURL (a postgres:// URL), verifies the
// connection, and applies the schema. It fails fast on an unreachable or
// unwilling database — a service configured for durable history must not
// silently fall back to amnesia.
func NewPostgresStore(ctx context.Context, databaseURL string, logger *slog.Logger) (*PostgresStore, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("conversation store: parse database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("conversation store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("conversation store: ping: %w", err)
	}
	for _, stmt := range schema {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			pool.Close()
			return nil, fmt.Errorf("conversation store: apply schema: %w", err)
		}
	}
	logger.Info("history.store", "type", "postgres", "host", cfg.ConnConfig.Host, "database", cfg.ConnConfig.Database)
	return &PostgresStore{pool: pool, maxRecentTurns: DefaultMaxRecentTurns}, nil
}

// Close releases the connection pool.
func (s *PostgresStore) Close() { s.pool.Close() }

// Turns implements [Store]: the conversation's newest turns (bounded by
// DefaultMaxRecentTurns), oldest first, reconstructed from the message rows
// by seq arithmetic. An incomplete pair at the window's old edge is dropped.
func (s *PostgresStore) Turns(ctx context.Context, projectName, contextID string) ([]Turn, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, role, content FROM messages
		 WHERE project_name = $1 AND context_id = $2
		 ORDER BY seq DESC LIMIT $3`,
		projectName, contextID, s.maxRecentTurns*2)
	if err != nil {
		return nil, fmt.Errorf("conversation store: load turns: %w", err)
	}
	defer rows.Close()

	byTurn := map[int64]*Turn{} // turn number (1-based) -> partial pair
	var order []int64
	for rows.Next() {
		var (
			seq           int64
			role, content string
		)
		if err := rows.Scan(&seq, &role, &content); err != nil {
			return nil, fmt.Errorf("conversation store: scan message: %w", err)
		}
		turnNo := (seq + 1) / 2
		t, seen := byTurn[turnNo]
		if !seen {
			t = &Turn{}
			byTurn[turnNo] = t
			order = append(order, turnNo)
		}
		if role == "user" {
			t.UserText = content
		} else {
			t.AssistantText = content
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversation store: load turns: %w", err)
	}

	// order is newest-first; emit oldest-first, skipping any half pair.
	turns := make([]Turn, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		t := byTurn[order[i]]
		if t.UserText == "" && t.AssistantText == "" {
			continue
		}
		turns = append(turns, *t)
	}
	if len(turns) == 0 {
		return nil, nil
	}
	return turns, nil
}

// Append implements [Store]: one transaction that bumps the conversation's
// turn count (creating it if new) and inserts the turn's two message rows.
// The RETURNING-ed turn count assigns seq, so concurrent appenders serialize
// on the conversation row and never collide.
func (s *PostgresStore) Append(ctx context.Context, projectName, contextID string, turn Turn) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("conversation store: begin append: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var turnNo int64
	err = tx.QueryRow(ctx,
		`INSERT INTO conversations (project_name, context_id, turn_count)
		 VALUES ($1, $2, 1)
		 ON CONFLICT (project_name, context_id) DO UPDATE
		   SET turn_count = conversations.turn_count + 1, last_active_at = now()
		 RETURNING turn_count`,
		projectName, contextID).Scan(&turnNo)
	if err != nil {
		return fmt.Errorf("conversation store: bump conversation: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO messages (project_name, context_id, seq, role, content)
		 VALUES ($1, $2, $3, 'user', $4), ($1, $2, $5, 'assistant', $6)`,
		projectName, contextID, 2*turnNo-1, turn.UserText, 2*turnNo, turn.AssistantText)
	if err != nil {
		return fmt.Errorf("conversation store: insert messages: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("conversation store: commit append: %w", err)
	}
	return nil
}

// ListConversations implements [Lister]: the project's conversations, newest
// activity first. limit <= 0 uses 100.
func (s *PostgresStore) ListConversations(ctx context.Context, projectName string, limit int) ([]Conversation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT project_name, context_id, created_at, last_active_at, turn_count
		 FROM conversations WHERE project_name = $1
		 ORDER BY last_active_at DESC LIMIT $2`,
		projectName, limit)
	if err != nil {
		return nil, fmt.Errorf("conversation store: list conversations: %w", err)
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ProjectName, &c.ContextID, &c.CreatedAt, &c.LastActiveAt, &c.TurnCount); err != nil {
			return nil, fmt.Errorf("conversation store: scan conversation: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversation store: list conversations: %w", err)
	}
	return out, nil
}
