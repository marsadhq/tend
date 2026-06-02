package store_test

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for the PG schema reset handle

	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// backend pairs a human-readable name with a freshly migrated Store.
type backend struct {
	name  string
	store store.Store
}

// pgSkipOnce ensures the "Postgres skipped" notice is logged at most once across
// the whole package test run rather than for every test that calls backends.
var pgSkipOnce sync.Once

// backends returns every store backend a test should run against, each opened
// fresh and migrated so tests are isolated and order-independent.
//
//   - SQLite is always included, backed by a temp file (not :memory:, whose
//     shared-cache path can mask real concurrency behaviour). A fresh temp dir
//     per call keeps tests isolated.
//   - Postgres is added when TEND_TEST_PG is set; its public schema is dropped
//     and recreated before migrating so each run starts from a clean slate.
//
// Cleanup (Close) is registered via t.Cleanup for every store returned.
func backends(t *testing.T) []backend {
	t.Helper()

	ctx := context.Background()
	var out []backend

	// SQLite: file-backed temp DB.
	sq, err := store.OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { sq.Close() })
	if err := sq.Migrate(ctx); err != nil {
		t.Fatalf("sqlite Migrate: %v", err)
	}
	out = append(out, backend{name: "sqlite", store: sq})

	// Postgres: only when a DSN is provided.
	dsn := os.Getenv("TEND_TEST_PG")
	if dsn == "" {
		pgSkipOnce.Do(func() { t.Log("TEND_TEST_PG not set; skipping Postgres backend") })
		return out
	}

	// Reset the schema on a throwaway connection so each run is clean and tests
	// are order-independent.
	resetDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg reset handle: %v", err)
	}
	if _, err := resetDB.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		resetDB.Close()
		t.Fatalf("reset pg schema: %v", err)
	}
	resetDB.Close()

	pg, err := store.OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	if err := pg.Migrate(ctx); err != nil {
		t.Fatalf("postgres Migrate: %v", err)
	}
	out = append(out, backend{name: "postgres", store: pg})

	return out
}

// forEachBackend runs fn as a subtest against every backend, opening a fresh,
// migrated store per backend so each subtest is isolated.
func forEachBackend(t *testing.T, fn func(t *testing.T, s store.Store)) {
	t.Helper()
	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) { fn(t, b.store) })
	}
}

// closeStore closes a Store via io.Closer. The Store interface intentionally
// does not expose Close, but both concrete backends implement it.
func closeStore(s store.Store) {
	if c, ok := s.(io.Closer); ok {
		_ = c.Close()
	}
}

// pgDSN returns the Postgres DSN from TEND_TEST_PG, skipping the calling test
// when it is unset.
func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEND_TEST_PG")
	if dsn == "" {
		t.Skip("TEND_TEST_PG not set; skipping Postgres-only test")
	}
	return dsn
}

// seedJob bootstraps the default org and creates one real shell job, returning
// the org and job IDs. Using a real job avoids any FK / missing-job surprises
// when enqueuing runs.
func seedJob(t *testing.T, ctx context.Context, s store.Store) (orgID, jobID int64) {
	t.Helper()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	id, err := s.CreateJob(ctx, jobs.Job{
		OrgID: org.ID, Name: "claimee", Type: jobs.Shell, Command: "echo hi", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return org.ID, id
}
