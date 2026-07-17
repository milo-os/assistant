package taskstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2astore "github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// TestBuildPoolConfigAppliesBounds pins that the durable task store is built with
// the same safety bounds as the conversation store — a capped MaxConns and a
// server-side statement_timeout — without needing a database.
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

func TestBuildPoolConfigRespectsOperatorStatementTimeout(t *testing.T) {
	cfg, err := buildPoolConfig("postgres://user:pw@localhost:5432/db?statement_timeout=42000")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != "42000" {
		t.Fatalf("statement_timeout = %q, want operator value 42000", got)
	}
}

// TestPageTokenRoundTrip pins the keyset token wire (base64 of RFC3339Nano_id),
// which must match the a2a-go in-memory store so paging is backend-agnostic.
func TestPageTokenRoundTrip(t *testing.T) {
	when := time.Date(2026, 7, 16, 10, 20, 30, 123456789, time.UTC)
	tok := encodePageToken(when, a2a.TaskID("task-abc"))
	gotTime, gotID, err := decodePageToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !gotTime.Equal(when) || gotID != "task-abc" {
		t.Fatalf("round trip = %v/%q, want %v/task-abc", gotTime, gotID, when)
	}
	if _, _, err := decodePageToken("!!not base64!!"); !errors.Is(err, a2a.ErrParseError) {
		t.Fatalf("bad token err = %v, want ErrParseError", err)
	}
}

// TestListRefusesUnscopedContext pins the tenant-safety default: the interface
// List with no [WithProjectScope] on the context must never leak — it returns an
// empty page without touching the database.
func TestListRefusesUnscopedContext(t *testing.T) {
	s := &PostgresStore{now: time.Now} // no pool: an unscoped List must not query
	resp, err := s.List(context.Background(), &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("unscoped List err = %v, want empty page", err)
	}
	if len(resp.Tasks) != 0 {
		t.Fatalf("unscoped List returned %d tasks, want 0", len(resp.Tasks))
	}
}

// ── DB-gated CRUD, tenant isolation, optimistic concurrency ──

func newTestStore(t *testing.T) *PostgresStore {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres task store test")
	}
	s, err := NewPostgresStore(context.Background(), url, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func taskFor(id, project, ctxID string) *a2a.Task {
	tsk := &a2a.Task{ID: a2a.TaskID(id), ContextID: ctxID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}
	tsk.SetMeta("projectName", project)
	return tsk
}

func TestPostgresStore_CreateGetUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	id := "task-cru-" + suffix

	task := taskFor(id, "proj-a-"+suffix, "ctx-"+suffix)
	v1, err := s.Create(ctx, task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v1 != a2astore.TaskVersion(1) {
		t.Fatalf("create version = %d, want 1", v1)
	}

	// Duplicate create is rejected.
	if _, err := s.Create(ctx, task); !errors.Is(err, a2astore.ErrTaskAlreadyExists) {
		t.Fatalf("dup create err = %v, want ErrTaskAlreadyExists", err)
	}

	got, err := s.Get(ctx, a2a.TaskID(id))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != v1 || got.Task.ID != a2a.TaskID(id) {
		t.Fatalf("get = %+v, want version %d id %s", got, v1, id)
	}

	// Update to working; version bumps.
	task.Status.State = a2a.TaskStateWorking
	v2, err := s.Update(ctx, &a2astore.UpdateRequest{Task: task, PrevVersion: v1})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if v2 != v1+1 {
		t.Fatalf("update version = %d, want %d", v2, v1+1)
	}

	// Stale PrevVersion is rejected (optimistic concurrency).
	if _, err := s.Update(ctx, &a2astore.UpdateRequest{Task: task, PrevVersion: v1}); !errors.Is(err, a2astore.ErrConcurrentModification) {
		t.Fatalf("stale update err = %v, want ErrConcurrentModification", err)
	}

	// Unknown task Get/Update surface ErrTaskNotFound.
	if _, err := s.Get(ctx, a2a.TaskID("nope-"+suffix)); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("get unknown err = %v, want ErrTaskNotFound", err)
	}
	if _, err := s.Update(ctx, &a2astore.UpdateRequest{Task: taskFor("nope-"+suffix, "p", "c")}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("update unknown err = %v, want ErrTaskNotFound", err)
	}
}

// TestPostgresStore_TenantIsolation is the cross-tenant regression pin: a
// ListForProjects scoped to tenant A's projects must NEVER return tenant B's
// tasks, and the interface List with A's scope must not either.
func TestPostgresStore_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	projA := "tenant-a-" + suffix
	projB := "tenant-b-" + suffix

	if _, err := s.Create(ctx, taskFor("a1-"+suffix, projA, "ctx-a-"+suffix)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, taskFor("a2-"+suffix, projA, "ctx-a-"+suffix)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, taskFor("b1-"+suffix, projB, "ctx-b-"+suffix)); err != nil {
		t.Fatal(err)
	}

	// Tenant A sees only its own two tasks.
	resp, err := s.ListForProjects(ctx, []string{projA}, &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(resp.Tasks) != 2 {
		t.Fatalf("tenant A saw %d tasks, want 2", len(resp.Tasks))
	}
	for _, tsk := range resp.Tasks {
		if projectOf(tsk) != projA {
			t.Fatalf("tenant A saw a task owned by %q", projectOf(tsk))
		}
	}

	// The interface List, scoped to A via context, must also exclude B.
	scoped := WithProjectScope(ctx, []string{projA}, false)
	iresp, err := s.List(scoped, &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("scoped List A: %v", err)
	}
	for _, tsk := range iresp.Tasks {
		if projectOf(tsk) == projB {
			t.Fatal("scoped interface List leaked a tenant-B task")
		}
	}

	// A caller with no grants sees nothing.
	none, err := s.ListForProjects(ctx, nil, &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("list none: %v", err)
	}
	if len(none.Tasks) != 0 {
		t.Fatalf("no-grant caller saw %d tasks, want 0", len(none.Tasks))
	}
}
