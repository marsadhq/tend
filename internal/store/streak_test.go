package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// streakT0 is when the failing streaks in these tests begin: the incident
// they are modelled on started on 2026-10-28 at 19:04 UTC.
var streakT0 = time.Date(2026, 10, 28, 19, 4, 0, 0, time.UTC)

// runAt enqueues, claims and finishes one run of jobID with status, recording
// its terminal event the way the runner does but stamped at, with detail as
// the runner's failure detail. claimant is who claimed it ("runner" for
// serve's runner, jobs.ManualClaimant() for `tend run`). It returns the run
// id.
func runAt(t *testing.T, ctx context.Context, s store.Store, orgID, jobID int64, claimant string, status jobs.RunStatus, at time.Time, detail string) int64 {
	t.Helper()
	runID, err := s.EnqueueRun(ctx, orgID, jobID)
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	run, ok, err := s.ClaimRun(ctx, claimant)
	if err != nil || !ok || run.ID != runID {
		t.Fatalf("ClaimRun = (%d, %v, %v), want run %d", run.ID, ok, err, runID)
	}
	typ, exit := jobs.EventRunFailed, 1
	if status == jobs.StatusSucceeded {
		typ, exit = jobs.EventRunSucceeded, 0
	}
	ev, err := jobs.RunEvent(run, "backup", typ, status, exit)
	if err != nil {
		t.Fatal(err)
	}
	ev.CreatedAt = at
	ev.Detail = detail
	if _, err := s.FinishRunAndEmit(ctx, runID, status, exit, 1, "", ev); err != nil {
		t.Fatalf("FinishRunAndEmit(%s): %v", status, err)
	}
	return runID
}

// reapAt claims a run of jobID and fails it through the reaper, stamped at.
func reapAt(t *testing.T, ctx context.Context, s store.Store, orgID, jobID int64, at time.Time) int64 {
	t.Helper()
	runID, err := s.EnqueueRun(ctx, orgID, jobID)
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	run, ok, err := s.ClaimRun(ctx, "runner")
	if err != nil || !ok || run.ID != runID {
		t.Fatalf("ClaimRun = (%d, %v, %v), want run %d", run.ID, ok, err, runID)
	}
	ev, err := jobs.RunEvent(run, "backup", jobs.EventRunFailed, jobs.StatusFailed, -1)
	if err != nil {
		t.Fatal(err)
	}
	ev.CreatedAt = at
	if reaped, err := s.ReapStaleRun(ctx, runID, "reaped: worker lost", ev); err != nil || !reaped {
		t.Fatalf("ReapStaleRun = %v, %v", reaped, err)
	}
	return runID
}

// runEvent returns the stored event of type typ about run runID.
func runEvent(t *testing.T, ctx context.Context, s store.Store, orgID, runID int64, typ string) core.Event {
	t.Helper()
	evs, err := s.ListEvents(ctx, orgID, 5000)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	key := fmt.Sprintf("run:%d:%s", runID, typ)
	for _, e := range evs {
		if e.DedupKey == key {
			return e
		}
	}
	t.Fatalf("no %s event for run %d", typ, runID)
	return core.Event{}
}

// streakOf reads the failing streak an event's detail records.
func streakOf(t *testing.T, e core.Event) jobs.Streak {
	t.Helper()
	var s jobs.Streak
	if e.Detail == "" {
		return s
	}
	if err := json.Unmarshal([]byte(e.Detail), &s); err != nil {
		t.Fatalf("%s detail %q: %v", e.Type, e.Detail, err)
	}
	return s
}

func wantStreak(t *testing.T, e core.Event, runs int, since time.Time) {
	t.Helper()
	got := streakOf(t, e)
	if got.FailedRuns != runs || !got.FailingSince.Equal(since) {
		t.Errorf("%s (%s) records %d failed runs since %v, want %d since %v (detail %s)",
			e.Type, e.DedupKey, got.FailedRuns, got.FailingSince, runs, since, e.Detail)
	}
}

// TestFailingStreakIsRecordedWithEachFailure: every run.failed records how
// many runs in a row have not succeeded and since when, whichever way the
// runs failed (an exit code, a timeout, the reaper, a definition that cannot
// be read); the run.recovered that ends the streak records the streak it
// ended; and the next failure starts a new one. The detail the runner wrote
// keeps its bytes, with the streak after them.
func TestFailingStreakIsRecordedWithEachFailure(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, jobID := seedJob(t, ctx, s)

		ok0 := runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusSucceeded, streakT0.Add(-5*time.Minute), "")
		if d := runEvent(t, ctx, s, orgID, ok0, jobs.EventRunSucceeded).Detail; d != "" {
			t.Errorf("run.succeeded detail = %q, want none", d)
		}

		f1 := runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, streakT0, `{"output":"connection refused"}`)
		e1 := runEvent(t, ctx, s, orgID, f1, jobs.EventRunFailed)
		wantStreak(t, e1, 1, streakT0)
		if !strings.HasPrefix(e1.Detail, `{"output":"connection refused",`) {
			t.Errorf("the runner's detail was not kept as it was: %s", e1.Detail)
		}

		f2 := runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusTimedOut, streakT0.Add(5*time.Minute), `{"timeout_seconds":30}`)
		wantStreak(t, runEvent(t, ctx, s, orgID, f2, jobs.EventRunFailed), 2, streakT0)

		f3 := reapAt(t, ctx, s, orgID, jobID, streakT0.Add(10*time.Minute))
		wantStreak(t, runEvent(t, ctx, s, orgID, f3, jobs.EventRunFailed), 3, streakT0)

		ok1 := runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusSucceeded, streakT0.Add(15*time.Minute), "")
		wantStreak(t, runEvent(t, ctx, s, orgID, ok1, jobs.EventRunRecovered), 3, streakT0)
		if d := runEvent(t, ctx, s, orgID, ok1, jobs.EventRunSucceeded).Detail; d != "" {
			t.Errorf("run.succeeded detail = %q, want none", d)
		}

		again := streakT0.Add(20 * time.Minute)
		f4 := runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, again, "")
		wantStreak(t, runEvent(t, ctx, s, orgID, f4, jobs.EventRunFailed), 1, again)

		// A definition that cannot be read fails the job the same way.
		ev, err := s.FailUnreadableJob(ctx, orgID, jobID, "claimee", "FAIL: the job's definition cannot be read")
		if err != nil {
			t.Fatalf("FailUnreadableJob: %v", err)
		}
		if got := streakOf(t, runEvent(t, ctx, s, orgID, notifyRunID(t, ev), jobs.EventRunFailed)); got.FailedRuns != 2 || !got.FailingSince.Equal(again) {
			t.Errorf("the unreadable job's failure records %+v, want 2 failed runs since %v", got, again)
		}
	})
}

// notifyRunID is the run id of a run event's payload.
func notifyRunID(t *testing.T, e core.Event) int64 {
	t.Helper()
	var p struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(e.Payload), &p); err != nil || p.RunID == 0 {
		t.Fatalf("payload %q names no run", e.Payload)
	}
	return p.RunID
}

// TestFailingStreakIsPerJob: one job's failures and successes leave
// another's streak alone.
func TestFailingStreakIsPerJob(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, a := seedJob(t, ctx, s)
		b, err := s.CreateJob(ctx, jobs.Job{OrgID: orgID, Name: "other", Type: jobs.Shell, Command: "true", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		runAt(t, ctx, s, orgID, a, "runner", jobs.StatusFailed, streakT0, "")
		runAt(t, ctx, s, orgID, b, "runner", jobs.StatusFailed, streakT0.Add(time.Minute), "")
		runAt(t, ctx, s, orgID, b, "runner", jobs.StatusSucceeded, streakT0.Add(2*time.Minute), "")
		fa := runAt(t, ctx, s, orgID, a, "runner", jobs.StatusFailed, streakT0.Add(5*time.Minute), "")
		wantStreak(t, runEvent(t, ctx, s, orgID, fa, jobs.EventRunFailed), 2, streakT0)
		fb := runAt(t, ctx, s, orgID, b, "runner", jobs.StatusFailed, streakT0.Add(6*time.Minute), "")
		wantStreak(t, runEvent(t, ctx, s, orgID, fb, jobs.EventRunFailed), 1, streakT0.Add(6*time.Minute))
	})
}

// TestFailingStreakGoesWithItsJob: a deleted job takes its streak with it,
// so a job created again under its name starts from nothing.
func TestFailingStreakGoesWithItsJob(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, jobID := seedJob(t, ctx, s)
		runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, streakT0, "")
		runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, streakT0.Add(5*time.Minute), "")
		if err := s.DeleteJob(ctx, orgID, jobID); err != nil {
			t.Fatalf("DeleteJob: %v", err)
		}
		again, err := s.CreateJob(ctx, jobs.Job{OrgID: orgID, Name: "claimee", Type: jobs.Shell, Command: "false", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		at := streakT0.Add(10 * time.Minute)
		f := runAt(t, ctx, s, orgID, again, "runner", jobs.StatusFailed, at, "")
		wantStreak(t, runEvent(t, ctx, s, orgID, f, jobs.EventRunFailed), 1, at)
	})
}

// reopenable is a database that can be opened more than once, as a restart
// of Tend does.
type reopenable struct {
	name string
	open func(t *testing.T) store.Store
}

// reopenables returns a SQLite file and, with TEND_TEST_PG, a freshly reset
// Postgres schema; each open is a new, migrated store on the same database,
// which the test closes to "stop" Tend.
func reopenables(t *testing.T) []reopenable {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restart.db")
	out := []reopenable{{name: "sqlite", open: func(t *testing.T) store.Store {
		t.Helper()
		s, err := store.OpenSQLite(path)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		return s
	}}}
	for _, b := range backends(t) {
		if b.name != "postgres" {
			continue
		}
		dsn := pgDSN(t)
		out = append(out, reopenable{name: "postgres", open: func(t *testing.T) store.Store {
			t.Helper()
			s, err := store.OpenPostgres(dsn)
			if err != nil {
				t.Fatalf("OpenPostgres: %v", err)
			}
			if err := s.Migrate(context.Background()); err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			return s
		}})
	}
	return out
}

// TestFailingStreakSurvivesARestart: the streak lives in the database, so a
// Tend restarted in the middle of one carries on counting.
func TestFailingStreakSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	for _, db := range reopenables(t) {
		t.Run(db.name, func(t *testing.T) {
			s := db.open(t)
			orgID, jobID := seedJob(t, ctx, s)
			runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, streakT0, "")
			runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, streakT0.Add(5*time.Minute), "")
			closeStore(s)

			s = db.open(t)
			defer closeStore(s)
			f := runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, streakT0.Add(10*time.Minute), "")
			wantStreak(t, runEvent(t, ctx, s, orgID, f, jobs.EventRunFailed), 3, streakT0)
		})
	}
}

// TestMigrationBackfillsFailingStreaks: upgrading Tend while a job is
// failing gives it the streak its stored runs show (the failed and timed-out
// runs since its last success, dated from the first of them), so its next
// failure counts on from there; a job whose last run succeeded, or that never
// ran, has none.
func TestMigrationBackfillsFailingStreaks(t *testing.T) {
	ctx := context.Background()
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			db := store.RawDB(s)
			orgID, failing := seedJob(t, ctx, s)
			healthy, err := s.CreateJob(ctx, jobs.Job{OrgID: orgID, Name: "healthy", Type: jobs.Shell, Command: "true", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			never, err := s.CreateJob(ctx, jobs.Job{OrgID: orgID, Name: "never", Type: jobs.Shell, Command: "true", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			allFailed, err := s.CreateJob(ctx, jobs.Job{OrgID: orgID, Name: "all-failed", Type: jobs.Shell, Command: "false", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			// Runs as an older Tend left them: a success, then three
			// failures for "claimee"; failures then a success for "healthy";
			// only failures for "all-failed"; a pending run is not finished.
			insert := func(jobID int64, status string, ended time.Time) {
				t.Helper()
				q := `INSERT INTO job_runs (org_id, job_id, status, attempt, exit_code, created_at, started_at, ended_at)
					VALUES (` + phs(b.name, 1, 8) + `)`
				at := store.FormatTime(ended)
				if _, err := db.ExecContext(ctx, q, orgID, jobID, status, 1, 1, at, at, at); err != nil {
					t.Fatalf("insert run: %v", err)
				}
			}
			insert(failing, "succeeded", streakT0.Add(-time.Hour))
			insert(failing, "failed", streakT0)
			insert(failing, "timed_out", streakT0.Add(5*time.Minute))
			insert(failing, "failed", streakT0.Add(10*time.Minute))
			insert(healthy, "failed", streakT0)
			insert(healthy, "succeeded", streakT0.Add(5*time.Minute))
			insert(allFailed, "failed", streakT0.Add(time.Minute))
			insert(allFailed, "failed", streakT0.Add(2*time.Minute))
			if _, err := s.EnqueueRun(ctx, orgID, failing); err != nil {
				t.Fatal(err)
			}

			// Back to the state before the backfill, and run it again.
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET failed_runs = 0, failing_since = NULL`); err != nil {
				t.Fatalf("reset streaks: %v", err)
			}
			if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 15`); err != nil {
				t.Fatalf("forget migration 15: %v", err)
			}
			if err := s.Migrate(ctx); err != nil {
				t.Fatalf("Migrate: %v", err)
			}

			read := func(jobID int64) (int, sql.NullString) {
				t.Helper()
				var n int
				var since sql.NullString
				if err := db.QueryRowContext(ctx, `SELECT failed_runs, failing_since FROM jobs WHERE id = `+ph(b.name, 1), jobID).Scan(&n, &since); err != nil {
					t.Fatalf("read streak: %v", err)
				}
				return n, since
			}
			if n, since := read(failing); n != 3 || since.String != store.FormatTime(streakT0) {
				t.Errorf("failing job backfilled with %d since %v, want 3 since %s", n, since, store.FormatTime(streakT0))
			}
			if n, since := read(allFailed); n != 2 || since.String != store.FormatTime(streakT0.Add(time.Minute)) {
				t.Errorf("job that never succeeded backfilled with %d since %v, want 2", n, since)
			}
			for name, id := range map[string]int64{"healthy": healthy, "never": never} {
				if n, since := read(id); n != 0 || since.Valid {
					t.Errorf("%s job backfilled with %d since %v, want none", name, n, since)
				}
			}

			// Its next failure counts on.
			run, ok, err := s.ClaimRun(ctx, "runner")
			if err != nil || !ok {
				t.Fatalf("ClaimRun: %v %v", ok, err)
			}
			ev, _ := jobs.RunEvent(run, "claimee", jobs.EventRunFailed, jobs.StatusFailed, 1)
			ev.CreatedAt = streakT0.Add(15 * time.Minute)
			if _, err := s.FinishRunAndEmit(ctx, run.ID, jobs.StatusFailed, 1, 1, "", ev); err != nil {
				t.Fatal(err)
			}
			wantStreak(t, runEvent(t, ctx, s, orgID, run.ID, jobs.EventRunFailed), 4, streakT0)
		})
	}
}

// TestFailingStreakStartsOverAfterASuccessItMissed: a failure after a
// success always starts a new streak, even when the stored streak did not
// see that success (a run recorded by v0.6 after a rollback, or by hand).
func TestFailingStreakStartsOverAfterASuccessItMissed(t *testing.T) {
	ctx := context.Background()
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			orgID, jobID := seedJob(t, ctx, s)
			runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, streakT0, "")
			runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, streakT0.Add(5*time.Minute), "")
			now := store.FormatTime(time.Now())
			if _, err := store.RawDB(s).ExecContext(ctx, `INSERT INTO job_runs (org_id, job_id, status, attempt, exit_code, created_at, started_at, ended_at)
				VALUES (`+phs(b.name, 1, 8)+`)`, orgID, jobID, "succeeded", 1, 0, now, now, now); err != nil {
				t.Fatal(err)
			}
			at := streakT0.Add(15 * time.Minute)
			f := runAt(t, ctx, s, orgID, jobID, "runner", jobs.StatusFailed, at, "")
			wantStreak(t, runEvent(t, ctx, s, orgID, f, jobs.EventRunFailed), 1, at)
		})
	}
}
