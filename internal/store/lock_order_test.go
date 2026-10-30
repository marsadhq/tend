package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// The transactions about one job take its rows in one order on Postgres:
// its runs, then the job, then its failure_alerts rows. These tests line up
// a job's deletion with a run finishing and with a held alert's release, the
// one paused inside its transaction until the other is waiting on a lock,
// and check that both finish without a deadlock.

// postgresOnly returns the Postgres backend, skipping without TEND_TEST_PG.
// (SQLite runs one write transaction at a time, so it cannot deadlock.)
func postgresOnly(t *testing.T) store.Store {
	t.Helper()
	pgDSN(t)
	for _, b := range backends(t) {
		if b.name == "postgres" {
			return b.store
		}
	}
	t.Skip("no Postgres backend")
	return nil
}

// pauseAt makes the next transaction that reaches point wait there: it
// returns a channel that is closed when one has, and a func that lets it go.
func pauseAt(t *testing.T, point string) (reached <-chan struct{}, release func()) {
	t.Helper()
	at, gone := make(chan struct{}), make(chan struct{})
	first := true
	restore := store.SetPauseAt(func(p string) {
		if p != point || !first {
			return
		}
		first = false
		close(at)
		<-gone
	})
	t.Cleanup(restore)
	return at, func() { close(gone) }
}

// waitForALockWait returns once a session of db's database is waiting on a
// lock, or after 5 seconds.
func waitForALockWait(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func noDeadlock(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		if strings.Contains(err.Error(), "deadlock") {
			t.Fatalf("%s deadlocked: %v", what, err)
		}
		t.Fatalf("%s: %v", what, err)
	}
}

// TestDeletingAJobWhileItsRunFinishes: the run has been finished and holds
// its row; the deletion starts and waits for it; the finish then takes the
// job's row for the streak. Both commit.
func TestDeletingAJobWhileItsRunFinishes(t *testing.T) {
	s := postgresOnly(t)
	ctx := context.Background()
	p := newPacing(t, s)
	p.channel("ops-telegram", kitnotify.Telegram, nil, jobs.EventRunFailed, jobs.EventRunRecovered, jobs.EventJobRemoved)
	p.fail(streakT0) // the job is failing, so its alert state exists too

	runID, err := s.EnqueueRun(ctx, p.org, p.job)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.ClaimRun(ctx, "runner")
	if err != nil || !ok || run.ID != runID {
		t.Fatalf("ClaimRun: %v %v", ok, err)
	}
	ev, err := jobs.RunEvent(run, "backup", jobs.EventRunFailed, jobs.StatusFailed, 1)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := pauseAt(t, store.PauseFinishRunLocked)
	finished := make(chan error, 1)
	go func() {
		_, err := s.FinishRunAndEmit(ctx, runID, jobs.StatusFailed, 1, 1, "", ev)
		finished <- err
	}()
	<-reached
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteJob(ctx, p.org, p.job) }()
	waitForALockWait(t, store.RawDB(s))
	release()
	noDeadlock(t, "the finish", <-finished)
	noDeadlock(t, "the deletion", <-deleted)
	if _, err := s.GetJob(ctx, p.org, p.job); err == nil {
		t.Fatal("the job is still there")
	}
}

// TestDeletingAJobWhileItsHeldAlertIsReleased: the release holds the job's
// row; the deletion removes the runs and waits for the job's row; the
// release then writes the job's alert state. Both commit, and the deletion
// leaves no alert state behind.
func TestDeletingAJobWhileItsHeldAlertIsReleased(t *testing.T) {
	s := postgresOnly(t)
	ctx := context.Background()
	p := newPacing(t, s)
	p.channel("ops-telegram", kitnotify.Telegram, nil, jobs.EventRunFailed, jobs.EventRunRecovered, jobs.EventJobRemoved)
	p.fail(streakT0)
	p.succeed(streakT0.Add(5 * time.Minute))
	p.fail(streakT0.Add(10 * time.Minute)) // held

	reached, release := pauseAt(t, store.PauseReleaseJobLocked)
	released := make(chan error, 1)
	go func() {
		_, err := s.ReleaseHeldAlerts(ctx, streakT0.Add(2*time.Hour))
		released <- err
	}()
	<-reached
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteJob(ctx, p.org, p.job) }()
	waitForALockWait(t, store.RawDB(s))
	release()
	noDeadlock(t, "the release", <-released)
	noDeadlock(t, "the deletion", <-deleted)
	var n int
	if err := store.RawDB(s).QueryRowContext(ctx, `SELECT COUNT(*) FROM failure_alerts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d failure_alerts rows left after the job was deleted", n)
	}
}
