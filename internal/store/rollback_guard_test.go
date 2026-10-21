package store_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// rawCommand reads a job's command column as an older Tend would see it.
func rawCommand(t *testing.T, b backend, id int64) string {
	t.Helper()
	var cmd string
	if err := store.RawDB(b.store).QueryRowContext(context.Background(),
		`SELECT COALESCE(command, '') FROM jobs WHERE id = `+ph(b.name, 1), id).Scan(&cmd); err != nil {
		t.Fatalf("read command: %v", err)
	}
	return cmd
}

// TestProbeRowsCarryTheRollbackGuard: a probe's or a drill's row holds the
// guard command in its command column whatever the job says, so a Tend that
// does not know the type (v0.4 runs it as a shell job) fails it loudly
// instead of running nothing and reporting success. This version reads the
// column back as empty and never runs it.
func TestProbeRowsCarryTheRollbackGuard(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatal(err)
			}
			specs := map[jobs.JobType]*jobs.Spec{
				jobs.TLS:    {Host: "example.com", WarnDays: 14},
				jobs.Domain: {Domain: "example.com", WarnDays: 30},
				jobs.Expiry: {ExpiresAt: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), WarnDays: 14},
				jobs.Drill:  {Restore: "true"},
			}
			for typ, spec := range specs {
				id, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: string(typ), Type: typ, Command: "echo user-supplied", Enabled: true, Spec: spec})
				if err != nil {
					t.Fatal(err)
				}
				if got := rawCommand(t, b, id); got != jobs.RollbackGuardCommand {
					t.Errorf("%s: stored command %q, want the guard", typ, got)
				}
				j, err := s.GetJob(ctx, org.ID, id)
				if err != nil {
					t.Fatal(err)
				}
				if j.Command != "" {
					t.Errorf("%s: read back command %q, want none", typ, j.Command)
				}
				j.Command = "echo changed"
				if err := s.UpdateJob(ctx, j); err != nil {
					t.Fatal(err)
				}
				if got := rawCommand(t, b, id); got != jobs.RollbackGuardCommand {
					t.Errorf("%s: command after update %q, want the guard", typ, got)
				}
			}
			// A shell job's command is its own, as always.
			id, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "hello", Type: jobs.Shell, Command: "echo hi", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := rawCommand(t, b, id); got != "echo hi" {
				t.Errorf("shell job command stored as %q", got)
			}
			if j, _ := s.GetJob(ctx, org.ID, id); j.Command != "echo hi" {
				t.Errorf("shell job command read as %q", j.Command)
			}
		})
	}
}

// TestRollbackGuardMigration: migration 0013 puts the guard into the rows of
// probes and drills created before it, and leaves every other job alone.
func TestRollbackGuardMigration(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			db := store.RawDB(b.store)
			org, err := b.store.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
			insert := `INSERT INTO jobs (org_id, name, type, command, enabled, created_at, updated_at) VALUES (` + phs(b.name, 1, 7) + `)`
			for _, row := range [][]string{{"old-drill", "drill", ""}, {"old-tls", "tls", ""}, {"shell", "shell", "echo hi"}, {"http", "http", ""}} {
				if _, err := db.ExecContext(ctx, insert, org.ID, row[0], row[1], row[2], 1, now, now); err != nil {
					t.Fatalf("insert %s: %v", row[0], err)
				}
			}
			sqlText, err := os.ReadFile("migrations/" + b.name + "/0013_probe_rollback_guard.sql")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(sqlText), "'"+jobs.RollbackGuardCommand+"'") {
				t.Fatalf("the migration does not write jobs.RollbackGuardCommand:\n%s", sqlText)
			}
			if _, err := db.ExecContext(ctx, string(sqlText)); err != nil {
				t.Fatalf("run migration: %v", err)
			}
			for name, want := range map[string]string{"old-drill": jobs.RollbackGuardCommand, "old-tls": jobs.RollbackGuardCommand, "shell": "echo hi", "http": ""} {
				var got string
				if err := db.QueryRowContext(ctx, `SELECT COALESCE(command, '') FROM jobs WHERE name = `+ph(b.name, 1), name).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("%s: command %q, want %q", name, got, want)
				}
			}
		})
	}
}
