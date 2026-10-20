package httpserver_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for the schema handle below

	"github.com/marsadhq/tend/internal/store"
)

// pgSkipOnce logs the "Postgres skipped" notice once per test binary.
var pgSkipOnce sync.Once

// pgSchemaSeq numbers the schemas this test binary creates.
var pgSchemaSeq atomic.Int64

// forEachStore runs fn as a subtest against every store backend, each opened
// fresh and migrated: SQLite always, and Postgres when TEND_TEST_PG is set. The
// Postgres store lives in a schema of its own (selected through search_path
// and dropped when the test ends), because `go test ./...` runs the store
// package's tests, which reset the public schema, at the same time.
func forEachStore(t *testing.T, fn func(t *testing.T, s store.Store)) {
	t.Helper()
	ctx := context.Background()

	t.Run("sqlite", func(t *testing.T) {
		s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "h.db"))
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("sqlite Migrate: %v", err)
		}
		fn(t, s)
	})

	dsn := os.Getenv("TEND_TEST_PG")
	if dsn == "" {
		pgSkipOnce.Do(func() { t.Log("TEND_TEST_PG not set; skipping Postgres backend") })
		return
	}
	t.Run("postgres", func(t *testing.T) {
		schema := fmt.Sprintf("httpserver_test_%d_%d", os.Getpid(), pgSchemaSeq.Add(1))
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open pg schema handle: %v", err)
		}
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema); err != nil {
			admin.Close()
			t.Fatalf("create pg schema %s: %v", schema, err)
		}
		// Registered before the store's Close below, so it runs after it.
		t.Cleanup(func() {
			_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
			admin.Close()
		})
		pg, err := store.OpenPostgres(withSearchPath(t, dsn, schema))
		if err != nil {
			t.Fatalf("OpenPostgres: %v", err)
		}
		t.Cleanup(func() { pg.Close() })
		if err := pg.Migrate(ctx); err != nil {
			t.Fatalf("postgres Migrate: %v", err)
		}
		fn(t, pg)
	})
}

// withSearchPath returns dsn with search_path set to schema, for a DSN in
// either the URL or the keyword/value form. The DSN itself is never printed: it
// carries a password.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	if !strings.Contains(dsn, "://") {
		return dsn + " search_path=" + schema
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("TEND_TEST_PG is not a valid URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
