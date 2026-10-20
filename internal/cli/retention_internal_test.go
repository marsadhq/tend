package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for the raw handle

	"github.com/marsadhq/tend/internal/config"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// sweepBackend is a migrated store plus a raw handle on the same database for
// backdating rows.
type sweepBackend struct {
	name string
	st   store.Store
	raw  *sql.DB
}

// sweepBackends opens SQLite, and Postgres in a schema of its own when
// TEND_TEST_PG is set (the store package's tests reset the public schema
// while this package runs).
func sweepBackends(t *testing.T) []sweepBackend {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sweep.db")
	sq, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sq.Close() })
	if err := sq.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rawSQ, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rawSQ.Close() })
	out := []sweepBackend{{"sqlite", sq, rawSQ}}

	dsn := os.Getenv("TEND_TEST_PG")
	if dsn == "" {
		return out
	}
	schema := fmt.Sprintf("cli_sweep_test_%d", os.Getpid())
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("TEND_TEST_PG is not a valid URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pg, err := store.OpenPostgres(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rawPG, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rawPG.Close() })
	return append(out, sweepBackend{"postgres", pg, rawPG})
}

// TestRetentionSweepHonoursTheConfiguredWindows: job runs are kept for the
// configured 90 days (the sweep used to prune them after 30), a zero window
// keeps that history forever, and deliveries go after their own window.
func TestRetentionSweepHonoursTheConfiguredWindows(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, b := range sweepBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			s := b.st
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatal(err)
			}
			backdate := func(table string, id int64, age time.Duration) {
				t.Helper()
				ts := now.Add(-age).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
				if _, err := b.raw.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET created_at = '%s' WHERE id = %d`, table, ts, id)); err != nil {
					t.Fatalf("backdate %s %d: %v", table, id, err)
				}
			}
			count := func(table string) int {
				t.Helper()
				var n int
				if err := b.raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}

			jobID, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "j", Type: jobs.Shell, Command: "true", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			finished := func(age time.Duration) {
				t.Helper()
				id, err := s.EnqueueRun(ctx, org.ID, jobID)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok, err := s.ClaimRun(ctx, "runner"); err != nil || !ok {
					t.Fatalf("ClaimRun: %v %v", ok, err)
				}
				if err := s.FinishRun(ctx, id, jobs.StatusSucceeded, 0, ""); err != nil {
					t.Fatal(err)
				}
				backdate("job_runs", id, age)
			}
			finished(60 * 24 * time.Hour)  // inside 90 days: kept
			finished(100 * 24 * time.Hour) // outside: pruned

			evID, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.started", Source: "test"})
			if err != nil {
				t.Fatal(err)
			}
			backdate("events", evID, 400*24*time.Hour) // events kept forever below

			chID, err := s.CreateChannel(ctx, notify.Channel{OrgID: org.ID, Name: "ops", Kind: notify.Webhook}, "blob")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: chID, EventType: "run.failed", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.failed", Source: "test"}); err != nil {
				t.Fatal(err)
			}
			ds, err := s.ClaimDueDeliveries(ctx, time.Now().Add(time.Minute), time.Minute, 10)
			if err != nil || len(ds) != 1 {
				t.Fatalf("ClaimDueDeliveries = %v, %v", ds, err)
			}
			if err := s.MarkDeliveryDelivered(ctx, ds[0].ID); err != nil {
				t.Fatal(err)
			}
			backdate("deliveries", ds[0].ID, 8*24*time.Hour)

			ret := config.Retention{Events: 0, JobRuns: 90 * 24 * time.Hour, Deliveries: 7 * 24 * time.Hour}
			retentionSweep(ctx, s, now, ret, slog.New(slog.NewTextHandler(io.Discard, nil)))

			if n := count("job_runs"); n != 1 {
				t.Errorf("job runs after the sweep = %d, want 1 (60 days old kept, 100 days old pruned)", n)
			}
			if n := count("events"); n != 2 {
				t.Errorf("events after the sweep = %d, want 2 (events are kept forever with a 0 window)", n)
			}
			if n := count("deliveries"); n != 0 {
				t.Errorf("deliveries after the sweep = %d, want 0", n)
			}
		})
	}
}
