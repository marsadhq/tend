package jobs_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for the schema handle below

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// pgSkipOnce logs the "Postgres skipped" notice once per test binary.
var pgSkipOnce sync.Once

// pgSchemaSeq numbers the schemas this test binary creates.
var pgSchemaSeq atomic.Int64

// forEachStore runs fn as a subtest against every store backend, each opened
// fresh and migrated: SQLite always (a file in a temp dir), and Postgres when
// TEND_TEST_PG is set.
//
// The store package's own tests reset the Postgres "public" schema before each
// of theirs, and `go test ./...` runs the two packages at the same time. The
// stores handed out here therefore live in a schema of their own, selected
// through search_path and dropped again when the test ends, so neither package
// can pull the tables out from under the other.
func forEachStore(t *testing.T, fn func(t *testing.T, s store.Store)) {
	t.Helper()
	ctx := context.Background()

	t.Run("sqlite", func(t *testing.T) {
		s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "r.db"))
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
		fn(t, openPostgresInOwnSchema(t, ctx, dsn))
	})
}

// openPostgresInOwnSchema creates a schema nothing else uses, opens a store
// whose connections resolve every table name inside it, and migrates it. The
// schema is dropped when the test ends.
func openPostgresInOwnSchema(t *testing.T, ctx context.Context, dsn string) store.Store {
	t.Helper()
	schema := fmt.Sprintf("jobs_test_%d_%d", os.Getpid(), pgSchemaSeq.Add(1))

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
	return pg
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

// seedOrg bootstraps the default org and returns its ID.
func seedOrg(t *testing.T, ctx context.Context, s store.Store) int64 {
	t.Helper()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	return org.ID
}

// faultStore wraps a real store and lets a test fail chosen calls. Each hook,
// when set, is asked first: a non-nil error is returned in place of the call,
// nil lets the call through to the real store. Hooks run on the runner's
// goroutines, so they must be safe to call from there.
type faultStore struct {
	store.Store

	onDueJobs         func() error
	onEnqueueRun      func() error
	onEmitEvent       func() error
	onClaimRun        func() error
	onListRunningRuns func() error
	onReapStaleRun    func(runID int64) error
	onFinish          func(runID int64) error
}

func (f *faultStore) DueJobs(ctx context.Context, now time.Time) ([]jobs.Job, error) {
	if f.onDueJobs != nil {
		if err := f.onDueJobs(); err != nil {
			return nil, err
		}
	}
	return f.Store.DueJobs(ctx, now)
}

func (f *faultStore) EnqueueRun(ctx context.Context, orgID, jobID int64) (int64, error) {
	if f.onEnqueueRun != nil {
		if err := f.onEnqueueRun(); err != nil {
			return 0, err
		}
	}
	return f.Store.EnqueueRun(ctx, orgID, jobID)
}

func (f *faultStore) EmitEvent(ctx context.Context, e core.Event) (int64, error) {
	if f.onEmitEvent != nil {
		if err := f.onEmitEvent(); err != nil {
			return 0, err
		}
	}
	return f.Store.EmitEvent(ctx, e)
}

func (f *faultStore) ClaimRun(ctx context.Context, worker string) (jobs.Run, bool, error) {
	if f.onClaimRun != nil {
		if err := f.onClaimRun(); err != nil {
			return jobs.Run{}, false, err
		}
	}
	return f.Store.ClaimRun(ctx, worker)
}

func (f *faultStore) ListRunningRuns(ctx context.Context) ([]jobs.Run, error) {
	if f.onListRunningRuns != nil {
		if err := f.onListRunningRuns(); err != nil {
			return nil, err
		}
	}
	return f.Store.ListRunningRuns(ctx)
}

func (f *faultStore) ReapStaleRun(ctx context.Context, runID int64, output string, ev core.Event) (bool, error) {
	if f.onReapStaleRun != nil {
		if err := f.onReapStaleRun(runID); err != nil {
			return false, err
		}
	}
	return f.Store.ReapStaleRun(ctx, runID, output, ev)
}

func (f *faultStore) FinishRunAndEmit(ctx context.Context, runID int64, status jobs.RunStatus, exitCode, attempt int, output string, ev core.Event) (int64, error) {
	if f.onFinish != nil {
		if err := f.onFinish(runID); err != nil {
			return 0, err
		}
	}
	return f.Store.FinishRunAndEmit(ctx, runID, status, exitCode, attempt, output, ev)
}

// logCapture collects what a runner logs, as one JSON object per line, and can
// be read while the runner is still writing to it.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// newLogCapture returns a logger for Runner.Logger and the capture behind it.
func newLogCapture() (*slog.Logger, *logCapture) {
	c := &logCapture{}
	return slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})), c
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// records returns every line logged so far, decoded. Numbers decode as float64.
func (c *logCapture) records(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	raw := c.buf.String()
	c.mu.Unlock()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// withMsg returns the logged records whose message is msg.
func (c *logCapture) withMsg(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range c.records(t) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// atLevel returns the logged records at the given level ("ERROR", "WARN", ...).
func (c *logCapture) atLevel(t *testing.T, level string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range c.records(t) {
		if rec["level"] == level {
			out = append(out, rec)
		}
	}
	return out
}

// waitFor polls cond until it holds, failing the test with what after 10s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
