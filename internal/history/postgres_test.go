package history

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
)

// TestBuildPoolConfigAppliesBounds pins that the pool is constructed with the
// safety bounds finding #8 requires — a capped MaxConns and a server-side
// statement_timeout — without needing a database. NewPostgresStore pings on
// open, so the config seam is the only place these can be asserted offline.
func TestBuildPoolConfigAppliesBounds(t *testing.T) {
	cfg, err := buildPoolConfig("postgres://user:pw@localhost:5432/db")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != poolMaxConns {
		t.Fatalf("MaxConns = %d, want %d", cfg.MaxConns, poolMaxConns)
	}
	if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != statementTimeout {
		t.Fatalf("statement_timeout = %q, want %q", got, statementTimeout)
	}
}

// TestBuildPoolConfigRespectsOperatorStatementTimeout pins that an operator's
// URL-supplied statement_timeout is not clobbered by our default.
func TestBuildPoolConfigRespectsOperatorStatementTimeout(t *testing.T) {
	cfg, err := buildPoolConfig("postgres://user:pw@localhost:5432/db?statement_timeout=42000")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != "42000" {
		t.Fatalf("statement_timeout = %q, want operator value 42000", got)
	}
}

// TestPostgresStoreEnforcesPerConversationCap pins finding #9's Postgres bound:
// once a conversation exceeds maxTurns, the oldest message pairs are deleted at
// append time so only the newest maxTurns survive. Gated on TEST_DATABASE_URL,
// matching the conformance suite; the store's maxTurns is lowered so the test
// stays fast.
func TestPostgresStoreEnforcesPerConversationCap(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres cap test")
	}
	ctx := context.Background()
	s, err := NewPostgresStore(ctx, url, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	const capTurns = 3
	s.maxTurns = capTurns
	project := "proj-cap-" + uniqueSuffix(t)
	contextID := "ctx-cap-" + uniqueSuffix(t)

	const total = capTurns + 4
	for i := 0; i < total; i++ {
		if err := s.Append(ctx, project, contextID, Turn{
			UserText:      fmt.Sprintf("u%d", i),
			AssistantText: fmt.Sprintf("a%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	turns, err := s.Turns(ctx, project, contextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != capTurns {
		t.Fatalf("got %d turns, want cap of %d", len(turns), capTurns)
	}
	// Only the newest `capTurns` turns remain, oldest-first.
	for i, turn := range turns {
		wantIdx := total - capTurns + i
		if turn.UserText != fmt.Sprintf("u%d", wantIdx) {
			t.Fatalf("turn %d = %q, want u%d", i, turn.UserText, wantIdx)
		}
	}
}

// TestPostgresStoreCapsStoredContentLength pins finding #9's content clamp in
// the durable path. Gated on TEST_DATABASE_URL.
func TestPostgresStoreCapsStoredContentLength(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres content-cap test")
	}
	ctx := context.Background()
	s, err := NewPostgresStore(ctx, url, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	project := "proj-clamp-" + uniqueSuffix(t)
	contextID := "ctx-clamp-" + uniqueSuffix(t)
	huge := make([]byte, MaxStoredContentLen+4096)
	for i := range huge {
		huge[i] = 'x'
	}
	if err := s.Append(ctx, project, contextID, Turn{UserText: string(huge), AssistantText: string(huge)}); err != nil {
		t.Fatal(err)
	}
	turns, err := s.Turns(ctx, project, contextID)
	if err != nil || len(turns) != 1 {
		t.Fatalf("got %d turns, %v; want 1", len(turns), err)
	}
	if len(turns[0].UserText) != MaxStoredContentLen || len(turns[0].AssistantText) != MaxStoredContentLen {
		t.Fatalf("content not capped: user=%d assistant=%d, want %d",
			len(turns[0].UserText), len(turns[0].AssistantText), MaxStoredContentLen)
	}
}
