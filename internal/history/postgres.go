package history

import (
	"context"
	"errors"
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

const (
	// dbOpTimeout bounds every database method internally. Each method wraps
	// its incoming context with this deadline BEFORE touching the pool, so a
	// wedged-but-TCP-alive backend errors out instead of blocking forever —
	// even when the caller hands us an uncancellable context (recordHistory
	// appends under context.WithoutCancel with no deadline of its own). This
	// mirrors the usage emitter, which pairs WithoutCancel with WithTimeout.
	dbOpTimeout = 15 * time.Second

	// poolMaxConns caps the connection pool. With a bounded MaxConns a stalled
	// backend can wedge at most this many acquirers; every further Acquire then
	// fails fast on the caller's (now always bounded) context instead of the
	// whole service silently draining into Acquire.
	poolMaxConns = 8

	// statementTimeout is the server-side ceiling on any single statement,
	// sent as a connection RuntimeParam. It is a shorter, backend-enforced
	// bound that fires before dbOpTimeout for the common case of a slow query,
	// yielding a clean error; dbOpTimeout is the client-side backstop for a
	// backend too wedged to honor it. Expressed in milliseconds (10s).
	statementTimeout = "10000"
)

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
	// The user-given name (Rename). Added as an ALTER rather than a column in
	// the CREATE above so databases created before it get it too — the same
	// migrate-on-open, idempotent posture as the rest of this list. Nullable:
	// NULL is "never named", which is not the same as named the empty string.
	`ALTER TABLE conversations ADD COLUMN IF NOT EXISTS name text`,
	// When the conversation was archived (see [Archiver]); NULL is "not
	// archived". A timestamp rather than a boolean because the API reports
	// when, and a nullable column needs no backfill for rows that predate it.
	`ALTER TABLE conversations ADD COLUMN IF NOT EXISTS archived_at timestamptz`,
	// How many times the conversation has been compacted (see
	// [Window.Compactions]). Compact renumbers turn positions, so it checks
	// this under its row lock and refuses a position read before another
	// compaction. Rows that predate it start at 0, which is what they would
	// have read anyway.
	`ALTER TABLE conversations ADD COLUMN IF NOT EXISTS compactions bigint NOT NULL DEFAULT 0`,
	// The default listing (archived_at IS NULL) is served by
	// conversations_by_project_activity above: it walks the project's rows
	// newest first and skips archived ones, which stay a small minority, so
	// the LIMIT is reached after touching barely more rows than it returns.
	// The archived listing is the opposite shape — a few rows buried among
	// many active ones — and would otherwise walk the whole project to find
	// them. This partial index holds only archived rows, so it costs nothing
	// for the conversations that are never archived.
	`CREATE INDEX IF NOT EXISTS conversations_archived_by_project_activity
		ON conversations (project_name, last_active_at DESC) WHERE archived_at IS NOT NULL`,
}

// Conversation is the stored metadata of one conversation, for per-project
// listing (newest activity first).
type Conversation struct {
	ProjectName  string
	ContextID    string
	CreatedAt    time.Time
	LastActiveAt time.Time
	TurnCount    int64
	// Title is what the conversation is about: the opening user message,
	// collapsed to one line and cut to MaxTitleLen runes. Empty when the
	// conversation has no ordinary user turn (only a compaction summary).
	Title string
	// Name is what the user called this conversation (see [Renamer]), empty
	// until they name one. It is kept alongside Title rather than replacing
	// it so a rename never destroys the derived label underneath — clients
	// show the name when set and fall back to the title.
	Name string
	// ArchivedAt is when the conversation was archived (see [Archiver]); the
	// zero time means it is not archived.
	ArchivedAt time.Time
}

// ListOptions narrows a [Lister.ListConversations] call.
type ListOptions struct {
	// Limit caps the rows returned; <= 0 uses the store's default (100).
	Limit int
	// Archived selects which side of the archive to list: false (the zero
	// value, and so the default) lists only conversations that are not
	// archived, true only those that are. There is deliberately no "both":
	// every consumer is a list a person scans, and an archive that still
	// shows up in the everyday list has not hidden anything.
	Archived bool
}

// Lister lists a project's conversations. It is a separate interface from
// [Store] because the agent loop never needs it — it exists for consumers
// (a conversation-list API) and for operational inspection.
type Lister interface {
	ListConversations(ctx context.Context, projectName string, opts ListOptions) ([]Conversation, error)
}

// firstUserMessageSQL is the correlated subquery both conversation reads use
// to fetch the opening user message for [Conversation.Title]: the lowest-seq
// user row that is not the compaction marker (after Compact, seq 1 is the
// summary turn's synthetic user row). It walks the (project, context, seq)
// primary key, so it costs one index probe per listed conversation, and it
// is folded to a title in Go by [TitleOf] rather than in SQL so the two
// stores truncate identically.
const firstUserMessageSQL = `COALESCE((
		SELECT m.content FROM messages m
		WHERE m.project_name = c.project_name AND m.context_id = c.context_id
		  AND m.role = 'user' AND m.content <> '` + summaryUserMarker + `'
		ORDER BY m.seq LIMIT 1), '')`

// PostgresStore is a durable [Store] on PostgreSQL. Safe for concurrent use;
// concurrent appends to the same conversation serialize on the conversation
// row. Construct with [NewPostgresStore], release with Close.
type PostgresStore struct {
	pool           *pgxpool.Pool
	maxRecentTurns int
	maxTurns       int // per-conversation retention cap, enforced at Append
}

var (
	_ Store   = (*PostgresStore)(nil)
	_ Lister  = (*PostgresStore)(nil)
	_ Reader  = (*PostgresStore)(nil)
	_ Renamer = (*PostgresStore)(nil)
	_ Editor  = (*PostgresStore)(nil)
)

// NewPostgresStore connects to databaseURL (a postgres:// URL), verifies the
// connection, and applies the schema. It fails fast on an unreachable or
// unwilling database — a service configured for durable history must not
// silently fall back to amnesia.
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
	return &PostgresStore{pool: pool, maxRecentTurns: DefaultMaxRecentTurns, maxTurns: MaxTurnsPerConversation}, nil
}

// buildPoolConfig parses databaseURL and layers on the pool/backend bounds that
// keep a wedged database from taking the service down: a capped MaxConns and a
// server-side statement_timeout. Pool acquisition itself is bounded by the
// per-method dbOpTimeout context rather than a pool field (pgxpool has none),
// so no acquirer waits forever. Operator-supplied values in the URL win.
func buildPoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("conversation store: parse database url: %w", err)
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

// Ping verifies the database connection is alive. Used by the conversations
// apiserver's readyz check.
func (s *PostgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Turns implements [Store]: the conversation's newest turns (bounded by
// DefaultMaxRecentTurns), oldest first, reconstructed from the message rows
// by seq arithmetic. An incomplete pair at the window's old edge is dropped.
func (s *PostgresStore) Turns(ctx context.Context, projectName, contextID string) ([]Turn, error) {
	w, err := s.Window(ctx, projectName, contextID)
	return w.Turns, err
}

// Window implements [Store]: [PostgresStore.Turns] plus each turn's position,
// which is its turn number (seq 2k is turn k's assistant row), and the
// conversation's compaction count. The count is read before the messages, in
// its own statement: a compaction landing between the two reads makes the
// count stale rather than the positions, so the worst case is a Compact
// refused with [ErrCompactStale], never one applied to the wrong turns.
func (s *PostgresStore) Window(ctx context.Context, projectName, contextID string) (Window, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	var compactions int64
	err := s.pool.QueryRow(ctx,
		`SELECT compactions FROM conversations WHERE project_name = $1 AND context_id = $2`,
		projectName, contextID).Scan(&compactions)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Window{}, fmt.Errorf("conversation store: load compaction count: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT seq, role, content FROM messages
		 WHERE project_name = $1 AND context_id = $2
		 ORDER BY seq DESC LIMIT $3`,
		projectName, contextID, s.maxRecentTurns*2)
	if err != nil {
		return Window{}, fmt.Errorf("conversation store: load turns: %w", err)
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
			return Window{}, fmt.Errorf("conversation store: scan message: %w", err)
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
		return Window{}, fmt.Errorf("conversation store: load turns: %w", err)
	}

	// order is newest-first; emit oldest-first, skipping a turn with no text.
	// A skipped turn leaves a gap in Positions, which is why they are
	// returned rather than left to be derived from Head.
	turns := make([]Turn, 0, len(order))
	positions := make([]int64, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		t := byTurn[order[i]]
		if t.UserText == "" && t.AssistantText == "" {
			continue
		}
		turns = append(turns, *t)
		positions = append(positions, order[i])
	}
	if len(turns) == 0 {
		return Window{Compactions: compactions}, nil
	}
	return Window{Turns: turns, Positions: positions, Head: positions[len(positions)-1], Compactions: compactions}, nil
}

// GetConversation implements [Reader]: one conversation's metadata by
// (project, context) key, or [ErrConversationNotFound] if absent.
func (s *PostgresStore) GetConversation(ctx context.Context, projectName, contextID string) (Conversation, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	var c Conversation
	var opening string
	var archivedAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT c.project_name, c.context_id, c.created_at, c.last_active_at, c.turn_count,
		        `+firstUserMessageSQL+`, COALESCE(c.name, ''), c.archived_at
		 FROM conversations c WHERE c.project_name = $1 AND c.context_id = $2`,
		projectName, contextID).Scan(&c.ProjectName, &c.ContextID, &c.CreatedAt, &c.LastActiveAt, &c.TurnCount, &opening, &c.Name, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, ErrConversationNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("conversation store: get conversation: %w", err)
	}
	c.Title = TitleOf(opening)
	if archivedAt != nil {
		c.ArchivedAt = *archivedAt
	}
	return c, nil
}

// Messages implements [Reader]: a conversation's message rows, oldest first.
// The underlying table always stores a summary turn as an ordinary
// ('user','assistant') row pair — the CHECK(role IN ('user','assistant'))
// constraint on the messages table admits nothing else, and Compact never
// needed to change that shape (see the design doc's read-path note): the
// summary marker lives in the row content, the same signal [IsSummaryTurn]
// uses in memory. So this pairs adjacent rows by seq (turn k is rows
// 2k-1/2k, same arithmetic [PostgresStore.Turns] uses) purely to recognize
// that marker, then renders a recognized pair as a single Role:"summary"
// message (dropping the synthetic user row) and everything else as the
// verbatim user/assistant rows it always was. Output seq is renumbered
// densely from 1 as messages are emitted, matching [MemoryStore.Messages].
func (s *PostgresStore) Messages(ctx context.Context, projectName, contextID string) ([]Message, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT seq, role, content, created_at FROM messages
		 WHERE project_name = $1 AND context_id = $2
		 ORDER BY seq ASC`,
		projectName, contextID)
	if err != nil {
		return nil, fmt.Errorf("conversation store: load messages: %w", err)
	}
	defer rows.Close()

	type rawRow struct {
		role, content string
		createdAt     time.Time
	}
	byTurn := map[int64][2]*rawRow{} // turn number (1-based) -> [user, assistant]
	var order []int64
	for rows.Next() {
		var (
			seq  int64
			r, c string
			ts   time.Time
		)
		if err := rows.Scan(&seq, &r, &c, &ts); err != nil {
			return nil, fmt.Errorf("conversation store: scan message: %w", err)
		}
		turnNo := (seq + 1) / 2
		pair, seen := byTurn[turnNo]
		if !seen {
			order = append(order, turnNo)
		}
		row := &rawRow{role: r, content: c, createdAt: ts}
		if seq%2 == 1 {
			pair[0] = row
		} else {
			pair[1] = row
		}
		byTurn[turnNo] = pair
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversation store: load messages: %w", err)
	}

	var out []Message
	outSeq := int64(0)
	for _, turnNo := range order {
		pair := byTurn[turnNo]
		user, assistant := pair[0], pair[1]
		if user != nil && user.content == summaryUserMarker && assistant != nil {
			outSeq++
			out = append(out, Message{Seq: outSeq, Role: "summary", Content: assistant.content, CreatedAt: assistant.createdAt})
			continue
		}
		if user != nil {
			outSeq++
			out = append(out, Message{Seq: outSeq, Role: user.role, Content: user.content, CreatedAt: user.createdAt})
		}
		if assistant != nil {
			outSeq++
			out = append(out, Message{Seq: outSeq, Role: assistant.role, Content: assistant.content, CreatedAt: assistant.createdAt})
		}
	}
	return out, nil
}

// Append implements [Store]: one transaction that bumps the conversation's
// turn count (creating it if new) and inserts the turn's two message rows.
// The RETURNING-ed turn count assigns seq, so concurrent appenders serialize
// on the conversation row and never collide. Over-long turn text is truncated
// to MaxStoredContentLen and the conversation is pruned to maxTurns, both in
// the same transaction, so a single conversation cannot grow without bound.
// The upsert also clears archived_at: a new turn in an archived conversation
// is the user taking it back out of the archive (see [Archiver]).
func (s *PostgresStore) Append(ctx context.Context, projectName, contextID string, turn Turn) error {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	turn = clampTurn(turn)
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
		   SET turn_count = conversations.turn_count + 1, last_active_at = now(), archived_at = NULL
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

	// Enforce the per-conversation retention cap in the same transaction:
	// once a conversation exceeds maxTurns, delete the oldest message pairs so
	// storage stays bounded. seq is monotonic (derived from turn_count, which
	// never decrements), so deleting old rows never collides with future ones.
	if s.maxTurns > 0 && turnNo > int64(s.maxTurns) {
		minKeptSeq := 2 * (turnNo - int64(s.maxTurns))
		if _, err = tx.Exec(ctx,
			`DELETE FROM messages
			 WHERE project_name = $1 AND context_id = $2 AND seq <= $3`,
			projectName, contextID, minKeptSeq); err != nil {
			return fmt.Errorf("conversation store: prune conversation: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("conversation store: commit append: %w", err)
	}
	return nil
}

// Compact implements [Store.Compact] by turn position, in one transaction.
// It first creates or locks the conversation row — the same row every Append
// upserts — so an Append that committed before it is visible here and kept,
// and an Append that comes after it waits and continues from the new
// turn_count. Inside that lock it refuses a caller whose window predates
// another compaction ([ErrCompactStale]), deletes the message rows of turns
// 1..through, moves the surviving rows down so the summary takes turn 1 and
// the rest follow densely, sets turn_count to match and counts the
// compaction. Creating the row before touching messages is what lets the
// messages foreign key hold on a conversation compacted before its first
// Append. archived_at is untouched:
// unlike a turn, compaction is not the user asking for the conversation back.
func (s *PostgresStore) Compact(ctx context.Context, projectName, contextID string, summary Turn, through, compactions int64) error {
	if through < 0 {
		return fmt.Errorf("conversation store: compact through %d: %w", through, ErrCompactRange)
	}
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	summary = clampTurn(summary)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("conversation store: begin compact: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var head, done int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO conversations (project_name, context_id, turn_count)
		 VALUES ($1, $2, 0)
		 ON CONFLICT (project_name, context_id) DO UPDATE SET last_active_at = now()
		 RETURNING turn_count, compactions`,
		projectName, contextID).Scan(&head, &done); err != nil {
		return fmt.Errorf("conversation store: compact: lock conversation: %w", err)
	}
	if compactions != done {
		return fmt.Errorf("conversation store: compact from compaction %d of %d: %w", compactions, done, ErrCompactStale)
	}
	if through > head {
		return fmt.Errorf("conversation store: compact through %d of %d: %w", through, head, ErrCompactRange)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM messages WHERE project_name = $1 AND context_id = $2 AND seq <= $3`,
		projectName, contextID, 2*through); err != nil {
		return fmt.Errorf("conversation store: compact: delete messages: %w", err)
	}

	// Turn k > through becomes turn k-through+1, i.e. seq drops by
	// 2*(through-1) (or rises by 2 when through is 0). The rows go through
	// negative seq first: moving them in one UPDATE can collide with a row
	// not yet moved, since the primary key is checked row by row.
	if shift := 2 * (through - 1); shift != 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE messages SET seq = -seq
			 WHERE project_name = $1 AND context_id = $2 AND seq > $3`,
			projectName, contextID, 2*through); err != nil {
			return fmt.Errorf("conversation store: compact: renumber messages: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE messages SET seq = -seq - $3
			 WHERE project_name = $1 AND context_id = $2 AND seq < 0`,
			projectName, contextID, shift); err != nil {
			return fmt.Errorf("conversation store: compact: renumber messages: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO messages (project_name, context_id, seq, role, content)
		 VALUES ($1, $2, 1, 'user', $3), ($1, $2, 2, 'assistant', $4)`,
		projectName, contextID, summary.UserText, summary.AssistantText); err != nil {
		return fmt.Errorf("conversation store: compact: insert summary: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE conversations SET turn_count = $3, compactions = compactions + 1
		 WHERE project_name = $1 AND context_id = $2`,
		projectName, contextID, head-through+1); err != nil {
		return fmt.Errorf("conversation store: compact: update conversation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("conversation store: compact: commit: %w", err)
	}
	return nil
}

// Rename implements [Renamer]: a single UPDATE of the conversation row, which
// must already exist (0 rows affected is [ErrConversationNotFound] — a rename
// never conjures a conversation the way Append's upsert does). A name that
// normalizes to empty is stored as NULL, so "never named" and "named, then
// cleared" read back identically. last_active_at is deliberately untouched:
// naming a conversation is not activity in it, and bumping it would reshuffle
// the newest-first listing under the user's cursor.
func (s *PostgresStore) Rename(ctx context.Context, projectName, contextID, name string) error {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	var value *string
	if normalized := NormalizeName(name); normalized != "" {
		value = &normalized
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations SET name = $3
		 WHERE project_name = $1 AND context_id = $2`,
		projectName, contextID, value)
	if err != nil {
		return fmt.Errorf("conversation store: rename conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// SetArchived implements [Archiver]: a single UPDATE of the conversation row,
// with the same must-already-exist and leave-last_active_at-alone rules as
// [PostgresStore.Rename]. COALESCE keeps an existing archive time, so
// archiving twice reports the first time rather than the latest.
func (s *PostgresStore) SetArchived(ctx context.Context, projectName, contextID string, archived bool) error {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations
		 SET archived_at = CASE WHEN $3 THEN COALESCE(archived_at, now()) ELSE NULL END
		 WHERE project_name = $1 AND context_id = $2`,
		projectName, contextID, archived)
	if err != nil {
		return fmt.Errorf("conversation store: archive conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// Delete implements [Deleter]: one DELETE of the conversation row. The
// messages go with it through the messages table's ON DELETE CASCADE foreign
// key, in the same statement, so there is no window in which a transcript
// survives its conversation.
func (s *PostgresStore) Delete(ctx context.Context, projectName, contextID string) error {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM conversations WHERE project_name = $1 AND context_id = $2`,
		projectName, contextID)
	if err != nil {
		return fmt.Errorf("conversation store: delete conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// ListConversations implements [Lister]: the project's conversations on the
// archived side opts selects, newest activity first. A limit <= 0 uses 100.
func (s *PostgresStore) ListConversations(ctx context.Context, projectName string, opts ListOptions) ([]Conversation, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	// The two filters are spelled as literal predicates rather than one
	// parameterized "($2 AND archived_at IS NOT NULL) OR ..." so the planner
	// can match each against the index built for it (see schema).
	archivedFilter := `c.archived_at IS NULL`
	if opts.Archived {
		archivedFilter = `c.archived_at IS NOT NULL`
	}
	rows, err := s.pool.Query(ctx,
		`SELECT c.project_name, c.context_id, c.created_at, c.last_active_at, c.turn_count,
		        `+firstUserMessageSQL+`, COALESCE(c.name, ''), c.archived_at
		 FROM conversations c WHERE c.project_name = $1 AND `+archivedFilter+`
		 ORDER BY c.last_active_at DESC LIMIT $2`,
		projectName, limit)
	if err != nil {
		return nil, fmt.Errorf("conversation store: list conversations: %w", err)
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		var opening string
		var archivedAt *time.Time
		if err := rows.Scan(&c.ProjectName, &c.ContextID, &c.CreatedAt, &c.LastActiveAt, &c.TurnCount, &opening, &c.Name, &archivedAt); err != nil {
			return nil, fmt.Errorf("conversation store: scan conversation: %w", err)
		}
		c.Title = TitleOf(opening)
		if archivedAt != nil {
			c.ArchivedAt = *archivedAt
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversation store: list conversations: %w", err)
	}
	return out, nil
}
