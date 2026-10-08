package jobs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
)

// TestReapOnceFailsStaleRunningRun proves the periodic reaper: a 'running' run
// whose started_at + timeout + slack has passed is failed, emits run.failed,
// and fires the EventSink - and a second sweep is a no-op (no double event).
func TestReapOnceFailsStaleRunningRun(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}

	staleID, err := s.CreateJob(ctx, jobs.Job{
		OrgID: org.ID, Name: "stale", Type: jobs.Shell, Command: "sleep 999",
		TimeoutSeconds: 1, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	freshID, err := s.CreateJob(ctx, jobs.Job{
		OrgID: org.ID, Name: "fresh", Type: jobs.Shell, Command: "sleep 999",
		TimeoutSeconds: 7200, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two claimed runs: ClaimRun stamps started_at with the real wall clock.
	for _, id := range []int64{staleID, freshID} {
		if _, err := s.EnqueueRun(ctx, org.ID, id); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.ClaimRun(ctx, "runner"); err != nil || !ok {
			t.Fatalf("ClaimRun: ok=%v err=%v", ok, err)
		}
	}

	// One hour later: the 1s-timeout run is far past timeout+slack; the
	// 7200s-timeout run is still within its window.
	fk := clock.NewFake(time.Now().Add(time.Hour))

	var mu sync.Mutex
	var fired []core.Event
	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk)
	r.EventSink = func(_ context.Context, ev core.Event) {
		mu.Lock()
		fired = append(fired, ev)
		mu.Unlock()
	}

	if err := r.ReapOnce(ctx); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}

	staleRuns, err := s.ListRuns(ctx, org.ID, staleID, 1)
	if err != nil || len(staleRuns) != 1 {
		t.Fatalf("ListRuns(stale): %v %d", err, len(staleRuns))
	}
	if staleRuns[0].Status != jobs.StatusFailed {
		t.Errorf("stale run status: got %s want failed", staleRuns[0].Status)
	}
	if !strings.Contains(staleRuns[0].Output, "reaped") {
		t.Errorf("stale run output does not mention reaping: %q", staleRuns[0].Output)
	}

	freshRuns, err := s.ListRuns(ctx, org.ID, freshID, 1)
	if err != nil || len(freshRuns) != 1 {
		t.Fatalf("ListRuns(fresh): %v %d", err, len(freshRuns))
	}
	if freshRuns[0].Status != jobs.StatusRunning {
		t.Errorf("fresh run status: got %s want running (must not be reaped)", freshRuns[0].Status)
	}

	evts, err := s.ListEvents(ctx, org.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !hasType(evts, "run.failed") {
		t.Error("no run.failed event emitted for reaped run")
	}
	mu.Lock()
	if len(fired) != 1 || fired[0].Type != "run.failed" {
		t.Errorf("EventSink: got %d events, want exactly one run.failed", len(fired))
	}
	mu.Unlock()

	// Idempotence: a second sweep finds no 'running' stale run and emits nothing.
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatalf("second ReapOnce: %v", err)
	}
	evts2, err := s.ListEvents(ctx, org.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(evts2) != len(evts) {
		t.Errorf("second sweep emitted events: %d -> %d", len(evts), len(evts2))
	}
}

// TestReapOnceWaitsForRetries proves the reaper counts retries. started_at is
// stamped once at claim, but a job with retries may legitimately run several
// attempts of its timeout each, so a run that is past ONE timeout (plus slack)
// is still inside its limit and must be left alone; only a run past the limit
// of all its attempts is reaped.
func TestReapOnceWaitsForRetries(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Two retries of a 10 minute timeout: three attempts plus pauses is a little
	// over 30 minutes.
	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID: org.ID, Name: "retrying", Type: jobs.Shell, Command: "sleep 999",
		TimeoutSeconds: 600, MaxRetries: 2, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueRun(ctx, org.ID, jobID); err != nil {
		t.Fatal(err)
	}
	// ClaimRun stamps started_at with the real wall clock.
	if _, ok, err := s.ClaimRun(ctx, "runner"); err != nil || !ok {
		t.Fatalf("ClaimRun: ok=%v err=%v", ok, err)
	}

	// 13 minutes in: past one timeout plus slack, in the middle of attempt 2.
	fk := clock.NewFake(time.Now().Add(13 * time.Minute))
	fired := 0
	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk)
	r.EventSink = func(context.Context, core.Event) { fired++ }

	if err := r.ReapOnce(ctx); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	runs, err := s.ListRuns(ctx, org.ID, jobID, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v %d", err, len(runs))
	}
	if runs[0].Status != jobs.StatusRunning {
		t.Fatalf("run status after 13m: got %s want running (still retrying, must not be reaped)", runs[0].Status)
	}
	evts, err := s.ListEvents(ctx, org.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if hasType(evts, "run.failed") || fired != 0 {
		t.Fatalf("run.failed emitted for a run still inside its retry limit (sink fired %d)", fired)
	}

	// 40 minutes in: past every attempt, every pause, and the slack.
	fk.Advance(27 * time.Minute)
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	runs, err = s.ListRuns(ctx, org.ID, jobID, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v %d", err, len(runs))
	}
	if runs[0].Status != jobs.StatusFailed || !strings.Contains(runs[0].Output, "reaped") {
		t.Errorf("run after 40m: status %s output %q, want failed and reaped", runs[0].Status, runs[0].Output)
	}
	if fired != 1 {
		t.Errorf("EventSink fired %d times, want 1", fired)
	}
}

// TestRunnerDropsResultOfReapedRun proves a terminal state is final. If the
// reaper fails a run while its worker is still executing, the worker's late
// result must not overwrite that state, emit a second terminal event, or reach
// the EventSink - and the worker must carry on without an error.
func TestRunnerDropsResultOfReapedRun(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The job blocks until the test releases it, so it is certainly still
	// executing when the run is failed underneath it, and then exits 0.
	release := filepath.Join(t.TempDir(), "release")
	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID: org.ID, Name: "slow", Type: jobs.Shell,
		Command:        "while [ ! -e '" + release + "' ]; do sleep 0.05; done",
		TimeoutSeconds: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueRun(ctx, org.ID, jobID); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	fired := 0
	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.RealClock{})
	r.EventSink = func(context.Context, core.Event) {
		mu.Lock()
		fired++
		mu.Unlock()
	}

	drained := make(chan error, 1)
	go func() { drained <- r.DrainOnce(ctx) }()

	// Wait for the worker to claim the run, then fail it underneath the worker
	// the way the reaper would.
	var runID int64
	for deadline := time.Now().Add(10 * time.Second); runID == 0; {
		running, err := s.ListRunningRuns(ctx)
		if err != nil {
			t.Fatalf("ListRunningRuns: %v", err)
		}
		if len(running) == 1 {
			runID = running[0].ID
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run was never claimed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	reaped, err := s.ReapStaleRun(ctx, runID, "reaped: worker lost", core.Event{
		OrgID: org.ID, Type: "run.failed", Source: "jobs.runner", Payload: `{"status":"failed"}`,
	})
	if err != nil || !reaped {
		t.Fatalf("ReapStaleRun: reaped=%v err=%v", reaped, err)
	}

	// Let the job finish: the worker now reports a success for a run that is
	// already failed.
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("DrainOnce after the run was reaped: %v", err)
	}

	runs, err := s.ListRuns(ctx, org.ID, jobID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v %d", err, len(runs))
	}
	if runs[0].Status != jobs.StatusFailed || runs[0].Output != "reaped: worker lost" {
		t.Errorf("reaped run was overwritten: status=%s output=%q", runs[0].Status, runs[0].Output)
	}
	evts, err := s.ListEvents(ctx, org.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if hasType(evts, "run.succeeded") {
		t.Error("late result emitted run.succeeded for a run that was already failed")
	}
	mu.Lock()
	if fired != 0 {
		t.Errorf("EventSink fired %d times for the dropped result, want 0", fired)
	}
	mu.Unlock()
}
