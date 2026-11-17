// Package testdb gives Postgres-backed tests one way to find their database.
//
// Locally, `go test ./...` must pass with no database, so a missing
// TEST_DATABASE_URL skips the test. A workflow that provides a database sets
// TEST_DATABASE_REQUIRED as well: there a missing URL means the workflow lost
// its database, and a skip would turn every Postgres test green without running
// it, so it fails instead. Workflows without a database, such as the shared
// ci-basic one, leave it unset and skip.
package testdb

import (
	"os"
	"testing"
)

// EnvVar names the variable that holds the Postgres connection URL.
const EnvVar = "TEST_DATABASE_URL"

// RequiredEnvVar names the variable a workflow sets when it provides a
// database, so that a missing URL fails instead of skipping.
const RequiredEnvVar = "TEST_DATABASE_REQUIRED"

// URL returns the test database URL. When it is unset, URL fails the test if
// RequiredEnvVar is set and skips it otherwise.
func URL(tb testing.TB) string {
	tb.Helper()
	url := os.Getenv(EnvVar)
	if url != "" {
		return url
	}
	if os.Getenv(RequiredEnvVar) != "" {
		tb.Fatalf("%s not set but %s is: the workflow must provide a Postgres database", EnvVar, RequiredEnvVar)
	}
	tb.Skipf("%s not set — skipping Postgres-backed test", EnvVar)
	return ""
}
