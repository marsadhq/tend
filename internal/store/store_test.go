package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for the verify-side handle

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

func TestMigrateAndJobRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		j := jobs.Job{OrgID: org.ID, Name: "nightly", Type: jobs.Shell, Command: "echo hi", Cron: "0 3 * * *", Enabled: true}
		id, err := s.CreateJob(ctx, j)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetJob(ctx, org.ID, id)
		if err != nil || got.Name != "nightly" || got.Type != jobs.Shell || got.Command != "echo hi" || !got.Enabled {
			t.Fatalf("round-trip failed: %v %+v", err, got)
		}
	})
}

func TestMigrateIdempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		// backends already migrated once; further migrates must be no-ops.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("second Migrate: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("third Migrate: %v", err)
		}
	})
}

func TestBootstrapDefaultOrgIdempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		a, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if a.ID != b.ID {
			t.Fatalf("BootstrapDefaultOrg not idempotent: %d != %d", a.ID, b.ID)
		}
		if a.Name != "default" {
			t.Fatalf("default org name = %q, want %q", a.Name, "default")
		}
	})
}

// TestBootstrapDefaultOrgConcurrent reproduces the TOCTOU race that could create
// duplicate "default" orgs. Many goroutines call BootstrapDefaultOrg behind a
// start barrier; every returned org ID must be identical and there must be no
// errors. Runs against both backends. (For SQLite the file-backed path is what
// exposes the race; :memory:'s shared-cache path can mask it.)
func TestBootstrapDefaultOrgConcurrent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		const goroutines = 64
		var (
			start sync.WaitGroup // start barrier: released once all goroutines are ready
			done  sync.WaitGroup
			mu    sync.Mutex
			ids   []int64
			errs  []error
		)
		start.Add(1)
		done.Add(goroutines)

		for i := 0; i < goroutines; i++ {
			go func() {
				defer done.Done()
				start.Wait() // block until the barrier is released
				org, err := s.BootstrapDefaultOrg(ctx)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				ids = append(ids, org.ID)
			}()
		}

		start.Done() // release all goroutines at once
		done.Wait()

		if len(errs) != 0 {
			t.Fatalf("BootstrapDefaultOrg returned %d error(s); first: %v", len(errs), errs[0])
		}
		if len(ids) != goroutines {
			t.Fatalf("collected %d ids, want %d", len(ids), goroutines)
		}
		for i, id := range ids {
			if id != ids[0] {
				t.Fatalf("ids not all identical: ids[%d]=%d != ids[0]=%d", i, id, ids[0])
			}
		}
	})
}

// TestBootstrapDefaultOrgConcurrentSQLiteRowCount is a SQLite-specific companion
// to the cross-backend concurrent test above: it opens a SECOND connection to
// the same file and asserts exactly one orgs row exists after the race. This
// second-connection row count is inherently SQLite-path-specific, so it stays a
// SQLite-only test.
func TestBootstrapDefaultOrgConcurrentSQLiteRowCount(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const goroutines = 64
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	done.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer done.Done()
			start.Wait()
			_, _ = s.BootstrapDefaultOrg(ctx)
		}()
	}
	start.Done()
	done.Wait()

	// Open a separate connection to the same file and assert exactly one org.
	verify, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open verify db: %v", err)
	}
	defer verify.Close()
	var count int
	if err := verify.QueryRowContext(ctx, `SELECT COUNT(*) FROM orgs`).Scan(&count); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if count != 1 {
		t.Fatalf("orgs count = %d, want 1", count)
	}
}

func TestGetJobOrgScoping(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "scoped", Type: jobs.Shell, Command: "x", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		// Correct org succeeds.
		if _, err := s.GetJob(ctx, org.ID, id); err != nil {
			t.Fatalf("GetJob with correct org: %v", err)
		}
		// Wrong org returns ErrNotFound.
		if _, err := s.GetJob(ctx, org.ID+999, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetJob wrong org: got %v, want ErrNotFound", err)
		}
	})
}

func TestGetJobByName(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)
		id, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "byname", Type: jobs.Shell, Command: "x", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetJobByName(ctx, org.ID, "byname")
		if err != nil {
			t.Fatalf("GetJobByName: %v", err)
		}
		if got.ID != id {
			t.Fatalf("GetJobByName id = %d, want %d", got.ID, id)
		}
		if _, err := s.GetJobByName(ctx, org.ID, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetJobByName missing: got %v, want ErrNotFound", err)
		}
	})
}

func TestEnvMapRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)
		id, err := s.CreateJob(ctx, jobs.Job{
			OrgID:   org.ID,
			Name:    "withenv",
			Type:    jobs.Shell,
			Command: "x",
			Enabled: true,
			Env:     map[string]string{"A": "1", "B": "two"},
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetJob(ctx, org.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Env) != 2 || got.Env["A"] != "1" || got.Env["B"] != "two" {
			t.Fatalf("env round-trip failed: %+v", got.Env)
		}

		// Empty env must round-trip to an empty (non-panicking) map or nil.
		id2, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "noenv", Type: jobs.Shell, Command: "x", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		got2, err := s.GetJob(ctx, org.ID, id2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got2.Env) != 0 {
			t.Fatalf("expected empty env, got %+v", got2.Env)
		}
	})
}

func TestTimestampsRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)
		runAt := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
		next := time.Date(2026, 6, 2, 3, 0, 0, 0, time.UTC)
		id, err := s.CreateJob(ctx, jobs.Job{
			OrgID:     org.ID,
			Name:      "timed",
			Type:      jobs.Shell,
			Command:   "x",
			Enabled:   true,
			RunAt:     runAt,
			NextRunAt: next,
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetJob(ctx, org.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if !got.RunAt.Equal(runAt) {
			t.Fatalf("RunAt = %v, want %v", got.RunAt, runAt)
		}
		if !got.NextRunAt.Equal(next) {
			t.Fatalf("NextRunAt = %v, want %v", got.NextRunAt, next)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatalf("CreatedAt/UpdatedAt should be set: %+v", got)
		}
	})
}

func TestUpdateJobAndList(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)
		id, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "u", Type: jobs.Shell, Command: "before", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		j, err := s.GetJob(ctx, org.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		j.Command = "after"
		j.Enabled = false
		j.NextRunAt = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		if err := s.UpdateJob(ctx, j); err != nil {
			t.Fatalf("UpdateJob: %v", err)
		}
		got, err := s.GetJob(ctx, org.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Command != "after" || got.Enabled {
			t.Fatalf("update did not persist: %+v", got)
		}

		list, err := s.ListJobs(ctx, org.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].ID != id {
			t.Fatalf("ListJobs = %+v", list)
		}
	})
}

func TestSecretsRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)

		if _, err := s.GetSecret(ctx, org.ID, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetSecret missing: got %v, want ErrNotFound", err)
		}

		if err := s.PutSecret(ctx, org.ID, "token", "cipher-1"); err != nil {
			t.Fatal(err)
		}
		v, err := s.GetSecret(ctx, org.ID, "token")
		if err != nil {
			t.Fatal(err)
		}
		if v != "cipher-1" {
			t.Fatalf("GetSecret = %q, want %q", v, "cipher-1")
		}

		// Upsert: same name updates.
		if err := s.PutSecret(ctx, org.ID, "token", "cipher-2"); err != nil {
			t.Fatal(err)
		}
		v, err = s.GetSecret(ctx, org.ID, "token")
		if err != nil {
			t.Fatal(err)
		}
		if v != "cipher-2" {
			t.Fatalf("after upsert GetSecret = %q, want %q", v, "cipher-2")
		}
	})
}

func TestEventsNewestFirst(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)

		id1, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.started", Source: "jobs.runner", Payload: `{"a":1}`})
		if err != nil {
			t.Fatal(err)
		}
		id2, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.succeeded", Source: "jobs.runner", Payload: `{"b":2}`})
		if err != nil {
			t.Fatal(err)
		}
		if id2 <= id1 {
			t.Fatalf("expected increasing ids, got %d then %d", id1, id2)
		}

		evts, err := s.ListEvents(ctx, org.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(evts) != 2 {
			t.Fatalf("ListEvents len = %d, want 2", len(evts))
		}
		// Newest first.
		if evts[0].ID != id2 || evts[1].ID != id1 {
			t.Fatalf("ordering wrong: got %d,%d want %d,%d", evts[0].ID, evts[1].ID, id2, id1)
		}
		if evts[0].Type != "run.succeeded" || evts[0].Payload != `{"b":2}` {
			t.Fatalf("event fields not round-tripped: %+v", evts[0])
		}
		if evts[0].CreatedAt.IsZero() {
			t.Fatalf("event CreatedAt should be set")
		}
	})
}

func TestEnqueueRunAndList(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)
		jobID, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "r", Type: jobs.Shell, Command: "x", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		runID, err := s.EnqueueRun(ctx, org.ID, jobID)
		if err != nil {
			t.Fatal(err)
		}
		runs, err := s.ListRuns(ctx, org.ID, jobID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 {
			t.Fatalf("ListRuns len = %d, want 1", len(runs))
		}
		if runs[0].ID != runID || runs[0].Status != jobs.StatusPending || runs[0].JobID != jobID {
			t.Fatalf("run not as expected: %+v", runs[0])
		}
	})
}

func TestClaimAndFinishRun(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)
		jobID, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "claim", Type: jobs.Shell, Command: "x", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		runID, err := s.EnqueueRun(ctx, org.ID, jobID)
		if err != nil {
			t.Fatal(err)
		}

		run, ok, err := s.ClaimRun(ctx, "worker-1")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("expected to claim a pending run")
		}
		if run.ID != runID || run.JobID != jobID || run.Attempt < 1 {
			t.Fatalf("claimed run wrong: %+v", run)
		}
		if run.Status != jobs.StatusRunning {
			t.Fatalf("claimed run status = %q, want running", run.Status)
		}

		// No more pending runs.
		_, ok, err = s.ClaimRun(ctx, "worker-1")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("expected no pending run on second claim")
		}

		// Finish it.
		if err := s.FinishRun(ctx, runID, jobs.StatusSucceeded, 0, "done"); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
		runs, err := s.ListRuns(ctx, org.ID, jobID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if runs[0].Status != jobs.StatusSucceeded || runs[0].Output != "done" {
			t.Fatalf("finished run wrong: %+v", runs[0])
		}
		if runs[0].StartedAt.IsZero() || runs[0].EndedAt.IsZero() {
			t.Fatalf("started/ended should be set: %+v", runs[0])
		}
	})
}

// TestRequeueOrphanedRuns proves crash recovery: a run left in 'running'
// (claimed but never finished, as after a crash) is reset to 'pending' and
// becomes claimable again. Runs against both backends.
func TestRequeueOrphanedRuns(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID, jobID := seedJob(t, ctx, s)

		runID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatalf("EnqueueRun: %v", err)
		}
		// Claim it so it sits in 'running' (simulating an in-flight run).
		claimed, ok, err := s.ClaimRun(ctx, "worker")
		if err != nil || !ok {
			t.Fatalf("ClaimRun: ok=%v err=%v", ok, err)
		}
		if claimed.ID != runID || claimed.Status != jobs.StatusRunning {
			t.Fatalf("claimed run = %+v, want id %d running", claimed, runID)
		}

		// Reconcile: the orphaned 'running' run is re-queued.
		n, err := s.RequeueOrphanedRuns(ctx)
		if err != nil {
			t.Fatalf("RequeueOrphanedRuns: %v", err)
		}
		if n != 1 {
			t.Fatalf("RequeueOrphanedRuns returned %d, want 1", n)
		}

		// It is pending again.
		runs, err := s.ListRuns(ctx, orgID, jobID, 10)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(runs) != 1 || runs[0].Status != jobs.StatusPending {
			t.Fatalf("after requeue runs = %+v, want one pending", runs)
		}

		// And claimable again.
		reclaimed, ok, err := s.ClaimRun(ctx, "worker")
		if err != nil || !ok {
			t.Fatalf("re-ClaimRun: ok=%v err=%v", ok, err)
		}
		if reclaimed.ID != runID {
			t.Fatalf("re-claimed run id = %d, want %d", reclaimed.ID, runID)
		}

		// A second reconcile with nothing left in flight... actually the reclaim
		// above put it back into 'running', so a fresh reconcile finds it again.
		n, err = s.RequeueOrphanedRuns(ctx)
		if err != nil {
			t.Fatalf("second RequeueOrphanedRuns: %v", err)
		}
		if n != 1 {
			t.Fatalf("second RequeueOrphanedRuns returned %d, want 1", n)
		}
	})
}

func TestDueJobs(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)
		now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)

		// Due: enabled, next_run in the past.
		dueID, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "due", Type: jobs.Shell, Command: "x", Enabled: true, NextRunAt: now.Add(-time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		// Not due: next_run in the future.
		if _, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "future", Type: jobs.Shell, Command: "x", Enabled: true, NextRunAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		// Not due: disabled.
		if _, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "disabled", Type: jobs.Shell, Command: "x", Enabled: false, NextRunAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		// Not due: no next_run (unscheduled).
		if _, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "unscheduled", Type: jobs.Shell, Command: "x", Enabled: true}); err != nil {
			t.Fatal(err)
		}

		due, err := s.DueJobs(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 || due[0].ID != dueID {
			t.Fatalf("DueJobs = %+v, want only id %d", due, dueID)
		}

		// Once a pending run exists for the due job, it must no longer be due.
		if _, err := s.EnqueueRun(ctx, org.ID, dueID); err != nil {
			t.Fatal(err)
		}
		due, err = s.DueJobs(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 0 {
			t.Fatalf("DueJobs after enqueue = %+v, want none (no-overlap guard)", due)
		}
	})
}

// TestDueJobsSubSecondBoundary guards the fixed-width timestamp encoding. next_run
// is stored as TEXT and DueJobs compares it with "<= ?" as a STRING, so lexical
// order must equal chronological order. The job's next_run is a whole-second time
// T (which time.RFC3339Nano would write WITHOUT a fractional part), and one of the
// query bounds is a sub-second time (T+500ms, which RFC3339Nano would write WITH a
// ".5"). Under the old variable-width encoding, "T+500ms" sorts BEFORE "T"
// lexically ("...05Z" > "...05.5..." is false), so DueJobs(T+500ms) would WRONGLY
// fail to return the job. The fixed-width layout makes both sides 9-digit, so the
// comparison is correct.
func TestDueJobsSubSecondBoundary(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, _ := s.BootstrapDefaultOrg(ctx)

		// Whole-second next_run (no fractional part under RFC3339Nano).
		T := time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC)
		dueID, err := s.CreateJob(ctx, jobs.Job{
			OrgID: org.ID, Name: "boundary", Type: jobs.Shell, Command: "x",
			Enabled: true, NextRunAt: T,
		})
		if err != nil {
			t.Fatal(err)
		}

		// At exactly T: due.
		due, err := s.DueJobs(ctx, T)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 || due[0].ID != dueID {
			t.Fatalf("DueJobs(T) = %+v, want only id %d", due, dueID)
		}

		// At T+500ms (a sub-second instant): still due. This is the case the old
		// RFC3339Nano lexical comparison got WRONG.
		due, err = s.DueJobs(ctx, T.Add(500*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 || due[0].ID != dueID {
			t.Fatalf("DueJobs(T+500ms) = %+v, want only id %d", due, dueID)
		}

		// At T-500ms (a sub-second instant before T): NOT due.
		due, err = s.DueJobs(ctx, T.Add(-500*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 0 {
			t.Fatalf("DueJobs(T-500ms) = %+v, want none", due)
		}
	})
}

// TestFinishRunAndEmit proves that FinishRunAndEmit atomically marks a run
// terminal and emits the lifecycle event in one transaction. Runs against both
// backends (SQLite and Postgres when TEND_TEST_PG is set).
func TestFinishRunAndEmit(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID, jobID := seedJob(t, ctx, s)

		runID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatalf("EnqueueRun: %v", err)
		}
		// Claim it -> 'running'.
		if _, ok, err := s.ClaimRun(ctx, "worker"); err != nil || !ok {
			t.Fatalf("ClaimRun: ok=%v err=%v", ok, err)
		}

		ev := core.Event{
			OrgID:    orgID,
			Type:     "run.succeeded",
			Source:   "jobs.runner",
			Payload:  `{"run_id":1,"status":"succeeded"}`,
			DedupKey: "run:1:run.succeeded",
		}
		// attempt=3 simulates a run the executor retried twice before this
		// terminal write; it must be persisted onto the run row.
		evID, err := s.FinishRunAndEmit(ctx, runID, jobs.StatusSucceeded, 0, 3, "done", ev)
		if err != nil {
			t.Fatalf("FinishRunAndEmit: %v", err)
		}
		if evID <= 0 {
			t.Fatalf("FinishRunAndEmit returned evID=%d, want > 0", evID)
		}

		// (a) Run must be terminal with status succeeded, output "done", and the
		// recorded attempt count from the terminal write.
		runs, err := s.ListRuns(ctx, orgID, jobID, 10)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("ListRuns len = %d, want 1", len(runs))
		}
		if runs[0].Status != jobs.StatusSucceeded {
			t.Fatalf("run status = %q, want succeeded", runs[0].Status)
		}
		if runs[0].Output != "done" {
			t.Fatalf("run output = %q, want %q", runs[0].Output, "done")
		}
		if runs[0].Attempt != 3 {
			t.Fatalf("run attempt = %d, want 3 (final attempt count must be persisted)", runs[0].Attempt)
		}

		// (b) ListEvents must contain the run.succeeded event with the returned ID.
		evts, err := s.ListEvents(ctx, orgID, 10)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		var found bool
		for _, e := range evts {
			if e.Type == "run.succeeded" && e.ID == evID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected run.succeeded event with id=%d in ListEvents, got %+v", evID, evts)
		}
	})
}

// TestOpenDispatcher checks the store.Open driver dispatcher.
func TestOpenDispatcher(t *testing.T) {
	ctx := context.Background()

	// sqlite: usable store.
	t.Run("sqlite", func(t *testing.T) {
		s, err := store.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
		if err != nil {
			t.Fatalf("Open sqlite: %v", err)
		}
		t.Cleanup(func() { closeStore(s) })
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		if _, err := s.BootstrapDefaultOrg(ctx); err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
	})

	// postgres: only when a DSN is configured.
	t.Run("postgres", func(t *testing.T) {
		dsn := pgDSN(t)
		s, err := store.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("Open postgres: %v", err)
		}
		t.Cleanup(func() { closeStore(s) })
	})

	// bogus driver: non-nil error.
	t.Run("bogus", func(t *testing.T) {
		if _, err := store.Open("bogus", ""); err == nil {
			t.Fatal("Open bogus: expected non-nil error, got nil")
		}
	})
}
