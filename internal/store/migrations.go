package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationsFS embed.FS

// dialect names a per-backend migration set. The value is also the subdirectory
// under migrations/ that holds that dialect's SQL files.
type dialect string

const (
	dialectSQLite   dialect = "sqlite"
	dialectPostgres dialect = "postgres"
)

// migration is a single embedded SQL migration file.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads and parses every embedded migration for a dialect in
// lexical (version) order.
func loadMigrations(d dialect) ([]migration, error) {
	dir := "migrations/" + string(d)
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %s: %w", dir, err)
	}

	var ms []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := parseVersion(e.Name())
		if err != nil {
			return nil, err
		}
		b, err := migrationsFS.ReadFile(dir + "/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// parseVersion extracts the leading numeric version from a filename such as
// "0001_init.sql" -> 1.
func parseVersion(name string) (int, error) {
	prefix := name
	if i := strings.IndexAny(name, "_."); i >= 0 {
		prefix = name[:i]
	}
	v, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("migration %q: cannot parse version prefix: %w", name, err)
	}
	return v, nil
}

// runMigrations applies any of the dialect's embedded migrations not yet
// recorded in schema_migrations. It is idempotent: each migration runs in its
// own transaction and records its version on success. Both SQLite and Postgres
// support transactional DDL, so the per-migration transaction is safe on both.
func runMigrations(ctx context.Context, db *sql.DB, d dialect) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	ms, err := loadMigrations(d)
	if err != nil {
		return err
	}

	for _, m := range ms {
		if applied[m.version] {
			continue
		}
		if err := applyMigration(ctx, db, d, m); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}
	return nil
}

// appliedVersions returns the set of migration versions already recorded.
func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

// applyMigration runs a single migration's SQL and records its version inside
// one transaction. The version-recording INSERT uses dialect-appropriate
// placeholders ($1,$2 for Postgres, ? for SQLite).
func applyMigration(ctx context.Context, db *sql.DB, d dialect, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}
	insert := `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`
	if d == dialectPostgres {
		insert = `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`
	}
	if _, err := tx.ExecContext(ctx, insert,
		m.version, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
