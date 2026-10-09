package jobs_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/secrets"
	"github.com/marsadhq/tend/internal/store"
)

// finishAttempts is how many times the runner tries to record a result before
// it gives up. Written as a literal so a changed schedule cannot hide here.
const finishAttempts = 4

// eventCounts returns how many events of each type the org has.
func eventCounts(t *testing.T, ctx context.Context, s store.Store, orgID int64) map[string]int {
	t.Helper()
	evts, err := s.ListEvents(ctx, orgID, 200)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	counts := map[string]int{}
	for _, e := range evts {
		counts[e.Type]++
	}
	return counts
}

// countingSink counts the terminal events a runner fires.
type countingSink struct {
	mu     sync.Mutex
	events []core.Event
}

func (c *countingSink) fire(_ context.Context, ev core.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *countingSink) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, e := range c.events {
		out[i] = e.Type
	}
	return out
}

// TestFinishIsRetriedUntilItSucceeds proves a result is not lost to a store
// that fails for a moment. The finish fails once, twice or three times and then
// goes through: the run must end up with the job's real result, exactly one
// terminal event and one sink call, and the worker must report no error. The
// pauses the runner asks for are numbered 1, 2, 3 in order.
func TestFinishIsRetriedUntilItSucceeds(t *testing.T) {
	for failures := 1; failures < finishAttempts; failures++ {
		t.Run(fmt.Sprintf("%d failures", failures), func(t *testing.T) {
			forEachStore(t, func(t *testing.T, s store.Store) {
				ctx := context.Background()
				orgID := seedOrg(t, ctx, s)
				jobID, err := s.CreateJob(ctx, jobs.Job{
					OrgID: orgID, Name: "flaky-store", Type: jobs.Shell, Command: "echo the real result", Enabled: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				runID, err := s.EnqueueRun(ctx, orgID, jobID)
				if err != nil {
					t.Fatal(err)
				}

				var calls atomic.Int64
				fs := &faultStore{Store: s, onFinish: func(int64) error {
					if calls.Add(1) <= int64(failures) {
						return errors.New("database is locked")
					}
					return nil
				}}
				var pauses []int
				sink := &countingSink{}
				logger, logs := newLogCapture()
				r := jobs.NewRunner(fs, jobs.NewExecutor(), nil, clock.RealClock{})
				r.EventSink = sink.fire
				r.Logger = logger
				r.FinishBackoff = func(attempt int) time.Duration {
					pauses = append(pauses, attempt)
					return 0
				}

				if err := r.DrainOnce(ctx); err != nil {
					t.Fatalf("DrainOnce: %v, want the retried finish to succeed", err)
				}
				if got := calls.Load(); got != int64(failures)+1 {
					t.Errorf("finish was attempted %d times, want %d", got, failures+1)
				}
				for i, n := range pauses {
					if n != i+1 {
						t.Errorf("pauses were asked for as %v, want 1..%d in order", pauses, failures)
						break
					}
				}
				if len(pauses) != failures {
					t.Errorf("%d pauses, want one per failed attempt (%d)", len(pauses), failures)
				}

				run, err := s.GetRun(ctx, orgID, runID)
				if err != nil {
					t.Fatalf("GetRun: %v", err)
				}
				if run.Status != jobs.StatusSucceeded || run.Output != "the real result\n" {
					t.Errorf("run = %s %q, want succeeded with the job's output", run.Status, run.Output)
				}
				counts := eventCounts(t, ctx, s, orgID)
				if counts["run.succeeded"] != 1 || counts["run.failed"] != 0 {
					t.Errorf("events = %v, want exactly one run.succeeded", counts)
				}
				if got := sink.types(); len(got) != 1 || got[0] != "run.succeeded" {
					t.Errorf("sink received %v, want one run.succeeded", got)
				}
				if errs := logs.atLevel(t, "ERROR"); len(errs) != 0 {
					t.Errorf("a finish that succeeded on retry logged errors: %v", errs)
				}
				retries := logs.withMsg(t, "runner: finish run failed, retrying")
				if len(retries) != failures {
					t.Errorf("logged %d retries, want %d: %v", len(retries), failures, logs.records(t))
				}
				for _, rec := range retries {
					if rec["level"] != "WARN" || rec["run"] != float64(runID) || rec["job"] != "flaky-store" {
						t.Errorf("retry line = %v, want WARN run=%d job=flaky-store", rec, runID)
					}
				}
			})
		})
	}
}

// TestFinishIsRetriedForAnUnresolvedSecret proves the other way a run ends,
// a secret that could not be resolved, gets the same retries: its run.failed
// survives a finish that fails once.
func TestFinishIsRetriedForAnUnresolvedSecret(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
		if err != nil {
			t.Fatalf("NewBox: %v", err)
		}
		jobID, err := s.CreateJob(ctx, jobs.Job{
			OrgID: orgID, Name: "no-secret", Type: jobs.Shell, Command: "echo $X", Enabled: true,
			Env: map[string]string{"X": "{{ secret.missing }}"},
		})
		if err != nil {
			t.Fatal(err)
		}
		runID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatal(err)
		}

		var calls atomic.Int64
		fs := &faultStore{Store: s, onFinish: func(int64) error {
			if calls.Add(1) == 1 {
				return errors.New("database is locked")
			}
			return nil
		}}
		sink := &countingSink{}
		r := jobs.NewRunner(fs, jobs.NewExecutor(), box, clock.RealClock{})
		r.EventSink = sink.fire
		r.FinishBackoff = func(int) time.Duration { return 0 }

		if err := r.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		run, err := s.GetRun(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != jobs.StatusFailed || !strings.Contains(run.Output, "secret resolution failed") {
			t.Errorf("run = %s %q, want failed on secret resolution", run.Status, run.Output)
		}
		if counts := eventCounts(t, ctx, s, orgID); counts["run.failed"] != 1 {
			t.Errorf("events = %v, want exactly one run.failed", counts)
		}
		if got := sink.types(); len(got) != 1 || got[0] != "run.failed" {
			t.Errorf("sink received %v, want one run.failed", got)
		}
	})
}

// TestFinishGivesUpAfterItsRetries proves the retries are bounded and that
// giving up is loud: after the last attempt the error is returned, an ERROR
// line names the run and its job, nothing is recorded or fired, and the run is
// left 'running' for the reaper or the next restart, as before.
func TestFinishGivesUpAfterItsRetries(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		jobID, err := s.CreateJob(ctx, jobs.Job{
			OrgID: orgID, Name: "disk-full", Type: jobs.Shell, Command: "echo hi", Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		runID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatal(err)
		}

		boom := errors.New("database or disk is full")
		var calls atomic.Int64
		fs := &faultStore{Store: s, onFinish: func(int64) error {
			calls.Add(1)
			return boom
		}}
		sink := &countingSink{}
		logger, logs := newLogCapture()
		r := jobs.NewRunner(fs, jobs.NewExecutor(), nil, clock.RealClock{})
		r.EventSink = sink.fire
		r.Logger = logger
		r.FinishBackoff = func(int) time.Duration { return 0 }

		if err := r.DrainOnce(ctx); !errors.Is(err, boom) {
			t.Fatalf("DrainOnce: %v, want the store error", err)
		}
		if got := calls.Load(); got != finishAttempts {
			t.Errorf("finish was attempted %d times, want %d", got, finishAttempts)
		}

		errs := logs.atLevel(t, "ERROR")
		if len(errs) != 1 {
			t.Fatalf("logged %d ERROR lines, want exactly 1: %v", len(errs), logs.records(t))
		}
		rec := errs[0]
		if rec["msg"] != "runner: finish run failed, result not recorded" ||
			rec["run"] != float64(runID) || rec["job"] != "disk-full" ||
			rec["status"] != "succeeded" || rec["finish_attempts"] != float64(finishAttempts) ||
			rec["err"] != boom.Error() {
			t.Errorf("ERROR line = %v, want run=%d job=disk-full status=succeeded finish_attempts=%d", rec, runID, finishAttempts)
		}

		run, err := s.GetRun(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != jobs.StatusRunning {
			t.Errorf("run status = %s, want it left running", run.Status)
		}
		counts := eventCounts(t, ctx, s, orgID)
		if counts["run.succeeded"] != 0 || counts["run.failed"] != 0 {
			t.Errorf("events = %v, want no terminal event", counts)
		}
		if got := sink.types(); len(got) != 0 {
			t.Errorf("sink received %v, want nothing", got)
		}
	})
}

// TestFinishDoesNotRetryARunThatIsNotRunning proves the errors that must not
// be retried are not. The store reports jobs.ErrRunNotRunning, or
// jobs.ErrRunGone for a run that was deleted, each on its own and wrapped, and
// the runner asks exactly once, pauses never and fires nothing. A run that is
// not running is no error for the caller; a run that is gone still is, without
// an ERROR line, because nothing was recorded.
func TestFinishDoesNotRetryARunThatIsNotRunning(t *testing.T) {
	for name, sentinel := range map[string]error{
		"bare":         jobs.ErrRunNotRunning,
		"wrapped":      fmt.Errorf("finish run and emit: %w", jobs.ErrRunNotRunning),
		"gone":         jobs.ErrRunGone,
		"gone wrapped": fmt.Errorf("%w: %w", store.ErrNotFound, jobs.ErrRunGone),
	} {
		t.Run(name, func(t *testing.T) {
			forEachStore(t, func(t *testing.T, s store.Store) {
				ctx := context.Background()
				orgID := seedOrg(t, ctx, s)
				jobID, err := s.CreateJob(ctx, jobs.Job{
					OrgID: orgID, Name: "gone", Type: jobs.Shell, Command: "echo hi", Enabled: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.EnqueueRun(ctx, orgID, jobID); err != nil {
					t.Fatal(err)
				}

				var calls atomic.Int64
				fs := &faultStore{Store: s, onFinish: func(int64) error {
					calls.Add(1)
					return sentinel
				}}
				sink := &countingSink{}
				logger, logs := newLogCapture()
				r := jobs.NewRunner(fs, jobs.NewExecutor(), nil, clock.RealClock{})
				r.EventSink = sink.fire
				r.Logger = logger
				paused := false
				r.FinishBackoff = func(int) time.Duration {
					paused = true
					return 0
				}

				err = r.DrainOnce(ctx)
				if errors.Is(sentinel, jobs.ErrRunGone) {
					if !errors.Is(err, jobs.ErrRunGone) {
						t.Fatalf("DrainOnce: %v, want jobs.ErrRunGone for a run that no longer exists", err)
					}
				} else if err != nil {
					t.Fatalf("DrainOnce: %v, want nil for a run that is not running", err)
				}
				if got := calls.Load(); got != 1 {
					t.Errorf("finish was attempted %d times, want exactly 1 (no retry)", got)
				}
				if paused {
					t.Error("the runner paused for a retry")
				}
				if got := sink.types(); len(got) != 0 {
					t.Errorf("sink received %v, want nothing", got)
				}
				if errs := logs.atLevel(t, "ERROR"); len(errs) != 0 {
					t.Errorf("logged errors: %v", errs)
				}
				if recs := logs.withMsg(t, "runner: finish run failed, retrying"); len(recs) != 0 {
					t.Errorf("logged a retry: %v", recs)
				}
			})
		})
	}
}

// TestFinishOfAReapedRunIsAttemptedOnce proves the same against the real
// stores: a run the reaper failed while its job was still executing gets one
// finish attempt, which the store refuses, and no retry.
func TestFinishOfAReapedRunIsAttemptedOnce(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		release := filepath.Join(t.TempDir(), "release")
		jobID, err := s.CreateJob(ctx, jobs.Job{
			OrgID: orgID, Name: "reaped", Type: jobs.Shell,
			Command:        "while [ ! -e '" + release + "' ]; do sleep 0.05; done",
			TimeoutSeconds: 60, Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		runID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatal(err)
		}

		var calls atomic.Int64
		fs := &faultStore{Store: s, onFinish: func(int64) error {
			calls.Add(1)
			return nil // let the real store answer
		}}
		r := jobs.NewRunner(fs, jobs.NewExecutor(), nil, clock.RealClock{})
		r.FinishBackoff = func(int) time.Duration { return 0 }
		drained := make(chan error, 1)
		go func() { drained <- r.DrainOnce(ctx) }()

		waitFor(t, "the run to be claimed", func() bool {
			running, err := s.ListRunningRuns(ctx)
			if err != nil {
				t.Fatalf("ListRunningRuns: %v", err)
			}
			return len(running) == 1
		})
		reaped, err := s.ReapStaleRun(ctx, runID, "reaped: worker lost", core.Event{
			OrgID: orgID, Type: "run.failed", Source: "jobs.runner", Payload: `{"status":"failed"}`,
		})
		if err != nil || !reaped {
			t.Fatalf("ReapStaleRun: reaped=%v err=%v", reaped, err)
		}
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := <-drained; err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}

		if got := calls.Load(); got != 1 {
			t.Errorf("finish was attempted %d times, want exactly 1", got)
		}
		run, err := s.GetRun(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != jobs.StatusFailed || run.Output != "reaped: worker lost" {
			t.Errorf("run = %s %q, want the reaper's state untouched", run.Status, run.Output)
		}
		counts := eventCounts(t, ctx, s, orgID)
		if counts["run.failed"] != 1 || counts["run.succeeded"] != 0 {
			t.Errorf("events = %v, want exactly one run.failed", counts)
		}
	})
}

// TestFinishRetryStopsWhenTheContextIsCancelled proves shutdown is not held up
// by the retries, and does not end them in silence. A cancellation that arrives
// during a pause ends the pause, one that arrives during an attempt ends the
// attempts; either way no further attempt is made, nothing is logged as an
// error, and one line names the run that is left 'running' with a result that
// was not recorded.
func TestFinishRetryStopsWhenTheContextIsCancelled(t *testing.T) {
	const shutdownMsg = "runner: result not recorded because of shutdown; the run is left running, to be requeued at the next start"
	boom := errors.New("database is locked")

	cases := []struct {
		name string
		// arrange sets the runner and the store up to fail the finish and to
		// cancel the context at the moment under test.
		arrange      func(r *jobs.Runner, fs *faultStore, calls *atomic.Int64, cancel context.CancelFunc)
		wantErr      error
		wantAttempts int64
	}{
		{
			name: "during a pause",
			arrange: func(r *jobs.Runner, fs *faultStore, calls *atomic.Int64, cancel context.CancelFunc) {
				fs.onFinish = func(int64) error {
					calls.Add(1)
					return boom
				}
				r.FinishBackoff = func(int) time.Duration {
					time.AfterFunc(20*time.Millisecond, cancel)
					return time.Hour // only the cancellation can end this pause
				}
			},
			wantErr:      boom,
			wantAttempts: 1,
		},
		{
			name: "during the first attempt",
			arrange: func(r *jobs.Runner, fs *faultStore, calls *atomic.Int64, cancel context.CancelFunc) {
				fs.onFinish = func(int64) error {
					calls.Add(1)
					cancel()
					return context.Canceled
				}
				r.FinishBackoff = func(int) time.Duration { return 0 }
			},
			wantErr:      context.Canceled,
			wantAttempts: 1,
		},
		{
			name: "during a later attempt",
			arrange: func(r *jobs.Runner, fs *faultStore, calls *atomic.Int64, cancel context.CancelFunc) {
				fs.onFinish = func(int64) error {
					if calls.Add(1) < 3 {
						return boom
					}
					cancel()
					return context.Canceled
				}
				r.FinishBackoff = func(int) time.Duration { return 0 }
			},
			wantErr:      context.Canceled,
			wantAttempts: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachStore(t, func(t *testing.T, s store.Store) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				orgID := seedOrg(t, ctx, s)
				jobID, err := s.CreateJob(ctx, jobs.Job{
					OrgID: orgID, Name: "stopping", Type: jobs.Shell, Command: "echo hi", Enabled: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				runID, err := s.EnqueueRun(ctx, orgID, jobID)
				if err != nil {
					t.Fatal(err)
				}

				var calls atomic.Int64
				fs := &faultStore{Store: s}
				logger, logs := newLogCapture()
				r := jobs.NewRunner(fs, jobs.NewExecutor(), nil, clock.RealClock{})
				r.Logger = logger
				tc.arrange(r, fs, &calls, cancel)

				done := make(chan error, 1)
				go func() { done <- r.DrainOnce(ctx) }()
				select {
				case err := <-done:
					if !errors.Is(err, tc.wantErr) {
						t.Errorf("DrainOnce: %v, want %v", err, tc.wantErr)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("DrainOnce did not return after the context was cancelled")
				}
				if got := calls.Load(); got != tc.wantAttempts {
					t.Errorf("finish was attempted %d times, want %d", got, tc.wantAttempts)
				}
				if errs := logs.atLevel(t, "ERROR"); len(errs) != 0 {
					t.Errorf("shutdown logged errors: %v", errs)
				}
				recs := logs.withMsg(t, shutdownMsg)
				if len(recs) != 1 {
					t.Fatalf("shutdown logged %d times, want once: %v", len(recs), logs.records(t))
				}
				rec := recs[0]
				if rec["level"] != "WARN" || rec["run"] != float64(runID) || rec["job"] != "stopping" ||
					rec["status"] != "succeeded" || rec["finish_attempt"] != float64(tc.wantAttempts) ||
					rec["err"] != tc.wantErr.Error() {
					t.Errorf("shutdown line = %v, want WARN run=%d job=stopping status=succeeded finish_attempt=%d err=%q",
						rec, runID, tc.wantAttempts, tc.wantErr)
				}

				// The run is left for the next start to requeue.
				run, err := s.GetRun(context.Background(), orgID, runID)
				if err != nil {
					t.Fatalf("GetRun: %v", err)
				}
				if run.Status != jobs.StatusRunning {
					t.Errorf("run status = %s, want it left running", run.Status)
				}
			})
		})
	}
}

// lostAckStore commits a finish and then reports an error for it, once: what a
// connection that drops between the commit and its acknowledgement looks like
// to the caller.
type lostAckStore struct {
	store.Store
	lose atomic.Bool
}

func (l *lostAckStore) FinishRunAndEmit(ctx context.Context, runID int64, status jobs.RunStatus, exitCode, attempt int, output string, ev core.Event) (int64, error) {
	id, err := l.Store.FinishRunAndEmit(ctx, runID, status, exitCode, attempt, output, ev)
	if err == nil && l.lose.CompareAndSwap(true, false) {
		return 0, errors.New("connection lost after commit")
	}
	return id, err
}

// TestFinishRetryThatFindsTheRunFinished covers a finish that was committed
// although it reported an error. The retry is refused, because the run is no
// longer 'running', and that must leave the recorded result alone: one terminal
// state, one terminal event, no error. It must not be logged as a discarded
// result either, since nothing was discarded.
func TestFinishRetryThatFindsTheRunFinished(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		jobID, err := s.CreateJob(ctx, jobs.Job{
			OrgID: orgID, Name: "acked-late", Type: jobs.Shell, Command: "echo recorded; exit 3", Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		runID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatal(err)
		}

		ls := &lostAckStore{Store: s}
		ls.lose.Store(true)
		logger, logs := newLogCapture()
		r := jobs.NewRunner(ls, jobs.NewExecutor(), nil, clock.RealClock{})
		r.Logger = logger
		r.FinishBackoff = func(int) time.Duration { return 0 }

		if err := r.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v, want nil: the result was recorded", err)
		}
		run, err := s.GetRun(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != jobs.StatusFailed || run.ExitCode != 3 || run.Output != "recorded\n" {
			t.Errorf("run = %s exit %d %q, want the job's result (failed, exit 3)", run.Status, run.ExitCode, run.Output)
		}
		if counts := eventCounts(t, ctx, s, orgID); counts["run.failed"] != 1 || counts["run.succeeded"] != 0 {
			t.Errorf("events = %v, want exactly one run.failed", counts)
		}

		if recs := logs.withMsg(t, "runner: finish run failed, retrying"); len(recs) != 1 {
			t.Errorf("logged %d retries, want 1: %v", len(recs), logs.records(t))
		}
		if recs := logs.withMsg(t, "runner: discarded late result of a run that is no longer running"); len(recs) != 0 {
			t.Errorf("a result that was recorded is logged as discarded: %v", recs)
		}
		recs := logs.withMsg(t, "runner: run was already finished when its finish was retried; this attempt wrote nothing")
		if len(recs) != 1 {
			t.Fatalf("the refused retry was logged %d times, want once: %v", len(recs), logs.records(t))
		}
		if rec := recs[0]; rec["level"] != "WARN" || rec["run"] != float64(runID) || rec["job"] != "acked-late" ||
			rec["status"] != "failed" || rec["finish_attempt"] != float64(2) {
			t.Errorf("refused-retry line = %v, want WARN run=%d job=acked-late status=failed finish_attempt=2", rec, runID)
		}
		if errs := logs.atLevel(t, "ERROR"); len(errs) != 0 {
			t.Errorf("logged errors: %v", errs)
		}
	})
}

// TestFinishOfADeletedJobsRunIsAttemptedOnce proves a run whose job was deleted
// while it was in flight does not hold its worker through the retries. Deleting
// a job removes its runs, so there is nothing to record the result on and no
// retry can change that: the finish is attempted once, the dropped result is
// logged, and the worker reports no error.
func TestFinishOfADeletedJobsRunIsAttemptedOnce(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		release := filepath.Join(t.TempDir(), "release")
		jobID, err := s.CreateJob(ctx, jobs.Job{
			OrgID: orgID, Name: "deleted", Type: jobs.Shell,
			Command:        "while [ ! -e '" + release + "' ]; do sleep 0.05; done",
			TimeoutSeconds: 60, Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		runID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatal(err)
		}

		var calls atomic.Int64
		fs := &faultStore{Store: s, onFinish: func(int64) error {
			calls.Add(1)
			return nil // let the real store answer
		}}
		sink := &countingSink{}
		logger, logs := newLogCapture()
		r := jobs.NewRunner(fs, jobs.NewExecutor(), nil, clock.RealClock{})
		r.EventSink = sink.fire
		r.Logger = logger
		paused := false
		r.FinishBackoff = func(int) time.Duration {
			paused = true
			return 0
		}
		drained := make(chan error, 1)
		go func() { drained <- r.DrainOnce(ctx) }()

		waitFor(t, "the run to be claimed", func() bool {
			running, err := s.ListRunningRuns(ctx)
			if err != nil {
				t.Fatalf("ListRunningRuns: %v", err)
			}
			return len(running) == 1
		})
		if err := s.DeleteJob(ctx, orgID, jobID); err != nil {
			t.Fatalf("DeleteJob: %v", err)
		}
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := <-drained; !errors.Is(err, jobs.ErrRunGone) || !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("DrainOnce: %v, want jobs.ErrRunGone and store.ErrNotFound for a run that no longer exists", err)
		}

		if got := calls.Load(); got != 1 {
			t.Errorf("finish was attempted %d times, want exactly 1", got)
		}
		if paused {
			t.Error("the runner paused for a retry")
		}
		recs := logs.withMsg(t, "runner: run no longer exists, result dropped")
		if len(recs) != 1 {
			t.Fatalf("the dropped result was logged %d times, want once: %v", len(recs), logs.records(t))
		}
		if rec := recs[0]; rec["level"] != "WARN" || rec["run"] != float64(runID) || rec["job"] != "deleted" || rec["status"] != "succeeded" {
			t.Errorf("dropped-result line = %v, want WARN run=%d job=deleted status=succeeded", rec, runID)
		}
		if errs := logs.atLevel(t, "ERROR"); len(errs) != 0 {
			t.Errorf("logged errors: %v", errs)
		}
		if recs := logs.withMsg(t, "runner: finish run failed, retrying"); len(recs) != 0 {
			t.Errorf("logged a retry: %v", recs)
		}
		if got := sink.types(); len(got) != 0 {
			t.Errorf("sink received %v, want nothing", got)
		}
		if _, err := s.GetRun(ctx, orgID, runID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetRun after the job was deleted: %v, want store.ErrNotFound", err)
		}
	})
}

// TestFinishSurvivesALockedSQLiteDatabase reproduces the failure itself, with
// no fake in the way: another connection holds the SQLite write lock for longer
// than busy_timeout while a job finishes. The first finish fails with
// SQLITE_BUSY; the lock is gone by the retry, which must record the real result.
func TestFinishSurvivesALockedSQLiteDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "locked.db")
	// The store's usual pragmas, with a busy_timeout short enough for a test.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(100)&_pragma=foreign_keys(ON)"
	s, err := store.OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	orgID := seedOrg(t, ctx, s)

	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID: orgID, Name: "locked-out", Type: jobs.Shell,
		Command:        "touch '" + started + "'; while [ ! -e '" + release + "' ]; do sleep 0.05; done; echo finished",
		TimeoutSeconds: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := s.EnqueueRun(ctx, orgID, jobID)
	if err != nil {
		t.Fatal(err)
	}

	// The "other process": a second handle on the same file.
	other, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	t.Cleanup(func() { other.Close() })
	lockConn, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("second connection: %v", err)
	}
	defer lockConn.Close()

	var unlockErr error
	var unlocked atomic.Int64
	logger, logs := newLogCapture()
	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.RealClock{})
	r.Logger = logger
	r.FinishBackoff = func(attempt int) time.Duration {
		// The first finish has failed: the other process lets go of the lock.
		if attempt == 1 {
			_, unlockErr = lockConn.ExecContext(ctx, "ROLLBACK")
			unlocked.Add(1)
		}
		return 0
	}

	drained := make(chan error, 1)
	go func() { drained <- r.DrainOnce(ctx) }()

	// Once the job is executing, every write the runner makes before the finish
	// is done. Take the write lock, then let the job end.
	waitFor(t, "the job to start", func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	if _, err := lockConn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := <-drained; err != nil {
		t.Fatalf("DrainOnce: %v, want the retry to record the result", err)
	}
	if unlockErr != nil {
		t.Fatalf("release the write lock: %v", unlockErr)
	}
	if unlocked.Load() != 1 {
		t.Fatalf("the first finish did not fail: the lock was released %d times, want 1", unlocked.Load())
	}
	retries := logs.withMsg(t, "runner: finish run failed, retrying")
	if len(retries) != 1 {
		t.Fatalf("logged %d retries, want 1: %v", len(retries), logs.records(t))
	}
	if msg, _ := retries[0]["err"].(string); !strings.Contains(msg, "SQLITE_BUSY") && !strings.Contains(msg, "locked") {
		t.Errorf("the first finish failed with %q, want a busy/locked database", msg)
	}

	run, err := s.GetRun(ctx, orgID, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != jobs.StatusSucceeded || run.Output != "finished\n" {
		t.Errorf("run = %s %q, want succeeded with the job's output", run.Status, run.Output)
	}
	if counts := eventCounts(t, ctx, s, orgID); counts["run.succeeded"] != 1 || counts["run.failed"] != 0 {
		t.Errorf("events = %v, want exactly one run.succeeded", counts)
	}
}
