package gapreport

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// preCapabilityKeyDDL is a verbatim snapshot of the table as it stands in
// staging today, BEFORE the capability_key column: the original seven columns
// plus the kind/evidence ALTERs already applied. It is written out rather
// than sliced off [schema] on purpose — a slice would silently follow future
// edits to schema and stop describing the state this migration actually has
// to survive.
func preCapabilityKeyDDL(schemaName string) []string {
	t := schemaName + ".capability_gap_report"
	return []string{
		`CREATE SCHEMA IF NOT EXISTS ` + schemaName,
		`CREATE TABLE IF NOT EXISTS ` + t + ` (
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
			ON ` + t + ` (provider_project, created_at DESC)`,
		`ALTER TABLE ` + t + ` ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'MissingCapability'`,
		`ALTER TABLE ` + t + ` ADD COLUMN IF NOT EXISTS evidence_tool text NOT NULL DEFAULT ''`,
		`ALTER TABLE ` + t + ` ADD COLUMN IF NOT EXISTS evidence_observed text NOT NULL DEFAULT ''`,
		`ALTER TABLE ` + t + ` ADD COLUMN IF NOT EXISTS evidence_contradicted_by text NOT NULL DEFAULT ''`,
	}
}

// withSearchPath points a connection URL at one Postgres schema, so the test
// gets a private copy of the table without needing rights to create a
// database on a shared server.
func withSearchPath(rawURL, schemaName string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schemaName)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// TestPostgresStoreMigratesPreCapabilityKeyTable is the check that matters
// for shipping this against staging: build the table as it exists there
// today, put real rows in it, then apply the new schema TWICE — the shape of
// migrate-on-open when two replicas start, or one restarts. Nothing may be
// rewritten, nothing may be lost, and the rows that predate keys must still
// reach the provider through the aggregate.
func TestPostgresStoreMigratesPreCapabilityKeyTable(t *testing.T) {
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres store tests (memory conformance still ran)")
	}
	ctx := context.Background()

	schemaName := "gapreport_pre_" + strings.ReplaceAll(uniqueSuffix(t), "-", "_")
	admin, err := pgxpool.New(ctx, rawURL)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(admin.Close)
	for _, stmt := range preCapabilityKeyDDL(schemaName) {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("build pre-migration table: %v", err)
		}
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schemaName+` CASCADE`) })

	// Three legacy rows, in the shape staging actually holds: the same gap
	// filed by three conversations, described three ways, with no key to
	// join them on.
	legacy := []struct{ id, ctxID, capability string }{
		{"legacy-1", "ctx-1", "time-series CPU/memory utilization metrics"},
		{"legacy-2", "ctx-2", "Instance/Workload resource usage metrics (CPU, memory) over a time window"},
		{"legacy-3", "ctx-3", "CPU/memory usage metrics for workloads over a time window"},
	}
	for _, r := range legacy {
		if _, err := admin.Exec(ctx,
			`INSERT INTO `+schemaName+`.capability_gap_report
			   (id, provider_project, service_name, consumer_project, context_id, capability, summary)
			 VALUES ($1, 'streamco-platform', 'streaming.streamco.example', 'demo-project', $2, $3, 'user was diagnosing lag')`,
			r.id, r.ctxID, r.capability); err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}
	}

	storeURL, err := withSearchPath(rawURL, schemaName)
	if err != nil {
		t.Fatal(err)
	}

	// Apply the migration twice. The second run is the real assertion: every
	// statement is ADD COLUMN / CREATE INDEX IF NOT EXISTS, so re-running it
	// must be a no-op rather than an error or a rewrite.
	for attempt := 1; attempt <= 2; attempt++ {
		s, err := NewPostgresStore(ctx, storeURL, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("migration attempt %d: %v", attempt, err)
		}
		s.Close()
	}

	s, err := NewPostgresStore(ctx, storeURL, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// The column arrived as a metadata-only default, so every legacy row has
	// an empty key rather than a NULL the scan would choke on.
	var nonEmpty int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM capability_gap_report WHERE capability_key IS DISTINCT FROM ''`).Scan(&nonEmpty); err != nil {
		t.Fatalf("inspect capability_key: %v", err)
	}
	if nonEmpty != 0 {
		t.Fatalf("%d legacy rows have a non-empty capability_key; the migration must not invent keys", nonEmpty)
	}

	reports, err := s.List(ctx, "streamco-platform")
	if err != nil || len(reports) != len(legacy) {
		t.Fatalf("List = %+v, %v; want %d legacy rows intact", reports, err, len(legacy))
	}
	for _, r := range reports {
		if r.Kind != KindMissingCapability || r.CapabilityKey != "" || r.Summary != "user was diagnosing lag" {
			t.Fatalf("legacy row came back changed: %+v", r)
		}
	}

	// The provider's aggregate view: keyless rows must still be there, and
	// must NOT have been merged into one entry on the strength of their prose.
	groups, err := s.Aggregate(ctx, "streamco-platform")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != len(legacy) {
		t.Fatalf("Aggregate = %d entries, want %d — one per unmerged legacy row", len(groups), len(legacy))
	}
	seen := map[string]bool{}
	for _, g := range groups {
		seen[g.Key] = true
		if g.CapabilityKey != "" || g.Conversations != 1 || g.Occurrences != 1 {
			t.Errorf("legacy aggregate entry = %+v; want a keyless group of one", g)
		}
	}
	for _, r := range legacy {
		if !seen[r.id] {
			t.Errorf("legacy row %s vanished from the provider's aggregate view", r.id)
		}
	}

	// And the point of the whole change: two keyed reports filed after the
	// migration land as one entry with a count of two, alongside the legacy
	// rows rather than instead of them.
	for i := range 2 {
		if _, err := s.Insert(ctx, InsertParams{
			ProviderProject: "streamco-platform", ServiceName: "streaming.streamco.example",
			ConsumerProject: "demo-project", ContextID: fmt.Sprintf("ctx-new-%d", i),
			CapabilityKey: "workload-metrics", Capability: "CPU/memory metrics over a window",
			Summary: "user was diagnosing lag",
		}); err != nil {
			t.Fatal(err)
		}
	}
	groups, err = s.Aggregate(ctx, "streamco-platform")
	if err != nil || len(groups) != len(legacy)+1 {
		t.Fatalf("Aggregate = %d entries (%v), want %d", len(groups), err, len(legacy)+1)
	}
	if groups[0].CapabilityKey != "workload-metrics" || groups[0].Conversations != 2 {
		t.Fatalf("Aggregate[0] = %+v; want workload-metrics with 2 conversations, ordered first", groups[0])
	}
}
