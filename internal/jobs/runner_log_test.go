package jobs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// TestRunnerLogsStoreErrors proves no store error on the scheduler tick, the
// reaper or the claim/finish path is swallowed in silence: each is logged once
// at ERROR, naming the job or run it belongs to, and is still returned to the
// caller as before.
func TestRunnerLogsStoreErrors(t *testing.T) {
	boom := errors.New("store is unavailable")

	cases := []struct {
		name    string
		arrange func(t *testing.T, ctx context.Context, s store.Store, fs *faultStore, orgID int64) (runID int64)
		act     func(ctx context.Context, r *jobs.Runner) error
		wantMsg string
		wantJob string // "" when the log line cannot know the job
		wantRun bool   // the line must carry the run ID arrange returned
	}{
		{
			name: "tick cannot list due jobs",
			arrange: func(_ *testing.T, _ context.Context, _ store.Store, fs *faultStore, _ int64) int64 {
				fs.onDueJobs = func() error { return boom }
				return 0
			},
			act:     func(ctx context.Context, r *jobs.Runner) error { return r.Tick(ctx) },
			wantMsg: "runner: scheduler tick: list due jobs",
		},
		{
			name: "tick cannot enqueue a due job",
			arrange: func(t *testing.T, ctx context.Context, s store.Store, fs *faultStore, orgID int64) int64 {
				if _, err := s.CreateJob(ctx, jobs.Job{
					OrgID: orgID, Name: "due", Type: jobs.Shell, Command: "echo hi",
					Cron: "0 3 * * *", Enabled: true, NextRunAt: time.Now().Add(-time.Minute),
				}); err != nil {
					t.Fatal(err)
				}
				fs.onEnqueueRun = func() error { return boom }
				return 0
			},
			act:     func(ctx context.Context, r *jobs.Runner) error { return r.Tick(ctx) },
			wantMsg: "runner: scheduler tick: enqueue run",
			wantJob: "due",
		},
		{
			name: "reaper cannot list running runs",
			arrange: func(_ *testing.T, _ context.Context, _ store.Store, fs *faultStore, _ int64) int64 {
				fs.onListRunningRuns = func() error { return boom }
				return 0
			},
			act:     func(ctx context.Context, r *jobs.Runner) error { return r.ReapOnce(ctx) },
			wantMsg: "runner: reaper: list running runs",
		},
		{
			name: "reaper cannot fail a stale run",
			arrange: func(t *testing.T, ctx context.Context, s store.Store, fs *faultStore, orgID int64) int64 {
				runID := claimRunOf(t, ctx, s, jobs.Job{
					OrgID: orgID, Name: "stale", Type: jobs.Shell, Command: "sleep 999",
					TimeoutSeconds: 1, Enabled: true,
				})
				fs.onReapStaleRun = func(int64) error { return boom }
				return runID
			},
			act:     func(ctx context.Context, r *jobs.Runner) error { return r.ReapOnce(ctx) },
			wantMsg: "runner: reaper: fail stale run",
			wantRun: true,
		},
		{
			name: "worker cannot claim a run",
			arrange: func(_ *testing.T, _ context.Context, _ store.Store, fs *faultStore, _ int64) int64 {
				fs.onClaimRun = func() error { return boom }
				return 0
			},
			act:     func(ctx context.Context, r *jobs.Runner) error { return r.DrainOnce(ctx) },
			wantMsg: "runner: claim run",
		},
		{
			name: "worker cannot record a result",
			arrange: func(t *testing.T, ctx context.Context, s store.Store, fs *faultStore, orgID int64) int64 {
				jobID, err := s.CreateJob(ctx, jobs.Job{
					OrgID: orgID, Name: "unrecorded", Type: jobs.Shell, Command: "echo hi", Enabled: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				runID, err := s.EnqueueRun(ctx, orgID, jobID)
				if err != nil {
					t.Fatal(err)
				}
				fs.onFinish = func(int64) error { return boom }
				return runID
			},
			act:     func(ctx context.Context, r *jobs.Runner) error { return r.DrainOnce(ctx) },
			wantMsg: "runner: finish run failed, result not recorded",
			wantJob: "unrecorded",
			wantRun: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachStore(t, func(t *testing.T, s store.Store) {
				ctx := context.Background()
				fs := &faultStore{Store: s}
				runID := tc.arrange(t, ctx, s, fs, seedOrg(t, ctx, s))

				logger, logs := newLogCapture()
				// One hour ahead, so a run claimed just now is long overdue.
				r := jobs.NewRunner(fs, noBackoffExecutor(), nil, clock.NewFake(time.Now().Add(time.Hour)))
				r.Logger = logger
				r.FinishBackoff = func(int) time.Duration { return 0 }

				if err := tc.act(ctx, r); !errors.Is(err, boom) {
					t.Fatalf("returned error = %v, want the store error", err)
				}
				errs := logs.atLevel(t, "ERROR")
				if len(errs) != 1 {
					t.Fatalf("logged %d ERROR lines, want exactly 1: %v", len(errs), logs.records(t))
				}
				rec := errs[0]
				if rec["msg"] != tc.wantMsg {
					t.Errorf("msg = %q, want %q", rec["msg"], tc.wantMsg)
				}
				if rec["err"] != boom.Error() {
					t.Errorf("err = %q, want %q", rec["err"], boom.Error())
				}
				if tc.wantJob != "" && rec["job"] != tc.wantJob {
					t.Errorf("job = %v, want %q", rec["job"], tc.wantJob)
				}
				if tc.wantRun && rec["run"] != float64(runID) {
					t.Errorf("run = %v, want %d", rec["run"], runID)
				}
			})
		})
	}
}

// TestRunnerLogsLostStartEvent proves a run.started event that could not be
// written is mentioned at WARN, not ERROR, and does not stop the run: the event
// is advisory and the terminal one is still recorded.
func TestRunnerLogsLostStartEvent(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		jobID, err := s.CreateJob(ctx, jobs.Job{
			OrgID: orgID, Name: "unannounced", Type: jobs.Shell, Command: "echo hi", Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		runID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatal(err)
		}

		fs := &faultStore{Store: s, onEmitEvent: func() error { return errors.New("store is unavailable") }}
		logger, logs := newLogCapture()
		r := jobs.NewRunner(fs, jobs.NewExecutor(), nil, clock.RealClock{})
		r.Logger = logger
		if err := r.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}

		run, err := s.GetRun(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != jobs.StatusSucceeded {
			t.Errorf("run status = %s, want succeeded", run.Status)
		}
		recs := logs.records(t)
		if len(recs) != 1 {
			t.Fatalf("logged %d lines, want exactly 1: %v", len(recs), recs)
		}
		rec := recs[0]
		if rec["level"] != "WARN" || rec["msg"] != "runner: record run.started" ||
			rec["run"] != float64(runID) || rec["job"] != "unannounced" {
			t.Errorf("log line = %v, want WARN \"runner: record run.started\" run=%d job=unannounced", rec, runID)
		}
	})
}

// TestRunnerDoesNotLogErrorsOfACancelledContext proves shutdown stays quiet:
// the store calls that a cancelled context interrupts still fail, but they are
// not reported as errors.
func TestRunnerDoesNotLogErrorsOfACancelledContext(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx, cancel := context.WithCancel(context.Background())
		seedOrg(t, ctx, s)
		cancel()

		logger, logs := newLogCapture()
		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.RealClock{})
		r.Logger = logger

		for name, call := range map[string]func(context.Context) error{
			"Tick": r.Tick, "ReapOnce": r.ReapOnce, "DrainOnce": r.DrainOnce,
		} {
			if err := call(ctx); err == nil {
				t.Errorf("%s on a cancelled context returned nil, want an error", name)
			}
		}
		if recs := logs.records(t); len(recs) != 0 {
			t.Errorf("logged during shutdown: %v", recs)
		}
	})
}

// TestRunnerLimitsLogOfAClaimThatKeepsFailing proves a store that cannot be
// claimed from does not fill the log. The first failed claim is logged at ERROR
// as it happens; the ones after it at most once a minute, with the number of
// failed claims so far; the first claim that works again is reported once; and
// a failure after that starts over.
func TestRunnerLimitsLogOfAClaimThatKeepsFailing(t *testing.T) {
	const (
		firstMsg  = "runner: claim run"
		repeatMsg = "runner: claim run is still failing"
		againMsg  = "runner: claim run works again"
	)
	boom := errors.New("store is unavailable")

	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedOrg(t, ctx, s)

		failing := true // read and written on this goroutine only
		fs := &faultStore{Store: s, onClaimRun: func() error {
			if failing {
				return boom
			}
			return nil
		}}
		clk := clock.NewFake(time.Now())
		logger, logs := newLogCapture()
		r := jobs.NewRunner(fs, jobs.NewExecutor(), nil, clk)
		r.Logger = logger

		// claim makes n attempts to claim a run; the queue is empty throughout.
		claim := func(n int) {
			t.Helper()
			for i := 0; i < n; i++ {
				if err := r.DrainOnce(ctx); errors.Is(err, boom) != failing {
					t.Fatalf("DrainOnce: %v with the store failing=%v", err, failing)
				}
			}
		}
		// logged checks how many lines of each kind there are so far, and that
		// there are no others; it returns the latest of each kind.
		logged := func(when string, first, still, again int) (lastStill, lastAgain map[string]any) {
			t.Helper()
			firsts, stills, agains := logs.withMsg(t, firstMsg), logs.withMsg(t, repeatMsg), logs.withMsg(t, againMsg)
			if len(firsts) != first || len(stills) != still || len(agains) != again {
				t.Fatalf("%s: %d first, %d still-failing and %d works-again lines, want %d, %d and %d: %v",
					when, len(firsts), len(stills), len(agains), first, still, again, logs.records(t))
			}
			if all := logs.records(t); len(all) != first+still+again {
				t.Fatalf("%s: unexpected log lines: %v", when, all)
			}
			for _, rec := range append(firsts, stills...) {
				if rec["level"] != "ERROR" || rec["err"] != boom.Error() {
					t.Errorf("%s: failure line = %v, want ERROR with the store error", when, rec)
				}
			}
			if still > 0 {
				lastStill = stills[still-1]
			}
			if again > 0 {
				lastAgain = agains[again-1]
			}
			return lastStill, lastAgain
		}

		claim(5)
		logged("five failed claims in a row", 1, 0, 0)
		clk.Advance(59 * time.Second)
		claim(5)
		logged("just under a minute after the first", 1, 0, 0)

		clk.Advance(time.Second)
		claim(5)
		rec, _ := logged("a minute after the first", 1, 1, 0)
		if rec["failed_claims"] != float64(11) {
			t.Errorf("still-failing line = %v, want failed_claims=11", rec)
		}

		clk.Advance(59 * time.Second)
		claim(1)
		logged("just under a minute after the last line", 1, 1, 0)
		clk.Advance(time.Second)
		claim(1)
		rec, _ = logged("a minute after the last line", 1, 2, 0)
		if rec["failed_claims"] != float64(17) {
			t.Errorf("still-failing line = %v, want failed_claims=17", rec)
		}

		failing = false
		claim(3)
		_, rec = logged("the store is back", 1, 2, 1)
		if rec["level"] != "INFO" || rec["failed_claims"] != float64(17) {
			t.Errorf("works-again line = %v, want INFO failed_claims=17", rec)
		}

		failing = true
		claim(2)
		logged("it fails again", 2, 2, 1)
	})
}

// TestStartLogsAFailingClaimOnce proves the same through Start, where it
// matters: several workers each poll a store that fails at once, and between
// them they log the failure a single time.
func TestStartLogsAFailingClaimOnce(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seedOrg(t, ctx, s)

		var failed atomic.Int64
		fs := &faultStore{Store: s, onClaimRun: func() error {
			failed.Add(1)
			return errors.New("store is unavailable")
		}}
		logger, logs := newLogCapture()
		r := jobs.NewRunner(fs, jobs.NewExecutor(), nil, clock.RealClock{})
		r.Logger = logger
		r.Workers = 3
		r.TickInterval = 2 * time.Millisecond // idle workers poll this often

		done := make(chan error, 1)
		go func() { done <- r.Start(ctx) }()
		waitFor(t, "many failed claims", func() bool { return failed.Load() >= 60 })
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}

		recs := logs.records(t)
		if len(recs) != 1 || recs[0]["level"] != "ERROR" || recs[0]["msg"] != "runner: claim run" {
			t.Errorf("%d failed claims logged %d lines, want the first one at ERROR and nothing else: %v",
				failed.Load(), len(recs), recs)
		}
	})
}

// TestStartLogsRequeuedRuns proves Start reports how many runs it found still
// 'running' from an earlier process and put back in the queue, and says nothing
// when there were none.
func TestStartLogsRequeuedRuns(t *testing.T) {
	const msg = "runner: requeued runs left running by an earlier process"

	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)

		start := func() (*logCapture, func()) {
			logger, logs := newLogCapture()
			r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.RealClock{})
			r.TickInterval = 20 * time.Millisecond
			r.Logger = logger
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- r.Start(runCtx) }()
			return logs, func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("Start: %v", err)
				}
			}
		}

		// Two runs claimed by a process that then went away.
		for _, name := range []string{"orphan-a", "orphan-b"} {
			claimRunOf(t, ctx, s, jobs.Job{OrgID: orgID, Name: name, Type: jobs.Shell, Command: "echo hi", Enabled: true})
		}
		logs, stop := start()
		waitFor(t, "the requeue log line", func() bool { return len(logs.withMsg(t, msg)) > 0 })
		// The requeued runs are picked up again and finished.
		waitFor(t, "the requeued runs to finish", func() bool {
			running, err := s.ListRunningRuns(ctx)
			if err != nil {
				t.Fatalf("ListRunningRuns: %v", err)
			}
			evts, err := s.ListEvents(ctx, orgID, 50)
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			succeeded := 0
			for _, e := range evts {
				if e.Type == "run.succeeded" {
					succeeded++
				}
			}
			return len(running) == 0 && succeeded == 2
		})
		stop()
		recs := logs.withMsg(t, msg)
		if len(recs) != 1 {
			t.Fatalf("requeue logged %d times, want once: %v", len(recs), recs)
		}
		if recs[0]["runs"] != float64(2) || recs[0]["level"] != "INFO" {
			t.Errorf("requeue line = %v, want runs=2 at INFO", recs[0])
		}

		// A start with nothing to requeue does not mention it.
		logs, stop = start()
		time.Sleep(50 * time.Millisecond)
		stop()
		if recs := logs.withMsg(t, msg); len(recs) != 0 {
			t.Errorf("requeue logged with nothing to requeue: %v", recs)
		}
	})
}

// TestReapOnceLogsEachReap proves every reaped run is logged with its run ID,
// its job and the reason, and that a run still inside its window is not.
func TestReapOnceLogsEachReap(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)

		stale := map[string]int64{}
		for _, name := range []string{"stale-a", "stale-b"} {
			stale[name] = claimRunOf(t, ctx, s, jobs.Job{
				OrgID: orgID, Name: name, Type: jobs.Shell, Command: "sleep 999", TimeoutSeconds: 1, Enabled: true,
			})
		}
		claimRunOf(t, ctx, s, jobs.Job{
			OrgID: orgID, Name: "fresh", Type: jobs.Shell, Command: "sleep 999", TimeoutSeconds: 7200, Enabled: true,
		})

		logger, logs := newLogCapture()
		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.NewFake(time.Now().Add(time.Hour)))
		r.Logger = logger
		if err := r.ReapOnce(ctx); err != nil {
			t.Fatalf("ReapOnce: %v", err)
		}

		recs := logs.withMsg(t, "runner: reaped stale run")
		if len(recs) != len(stale) {
			t.Fatalf("logged %d reaps, want %d: %v", len(recs), len(stale), logs.records(t))
		}
		for _, rec := range recs {
			name, _ := rec["job"].(string)
			wantRun, ok := stale[name]
			if !ok {
				t.Errorf("reap logged for job %q, which was not stale", name)
				continue
			}
			if rec["run"] != float64(wantRun) {
				t.Errorf("job %s: run = %v, want %d", name, rec["run"], wantRun)
			}
			if rec["level"] != "WARN" {
				t.Errorf("job %s: level = %v, want WARN", name, rec["level"])
			}
			if reason, _ := rec["reason"].(string); !strings.Contains(reason, "time limit") {
				t.Errorf("job %s: reason = %q, want it to explain the time limit", name, reason)
			}
			// The reason is what the run's output says after "reaped: ".
			run, err := s.GetRun(ctx, orgID, wantRun)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if reason, _ := rec["reason"].(string); run.Output != "reaped: "+reason {
				t.Errorf("job %s: output %q does not match the logged reason %q", name, run.Output, reason)
			}
		}
	})
}

// TestRunnerLogsDiscardedLateResult proves a result that arrives after its run
// was failed underneath the worker is not dropped in silence: the discard is
// logged at WARN with the run, the job and the status that was thrown away.
func TestRunnerLogsDiscardedLateResult(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)

		release := filepath.Join(t.TempDir(), "release")
		jobID, err := s.CreateJob(ctx, jobs.Job{
			OrgID: orgID, Name: "late", Type: jobs.Shell,
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

		logger, logs := newLogCapture()
		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.RealClock{})
		r.Logger = logger
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

		recs := logs.withMsg(t, "runner: discarded late result of a run that is no longer running")
		if len(recs) != 1 {
			t.Fatalf("discard logged %d times, want once: %v", len(recs), logs.records(t))
		}
		rec := recs[0]
		if rec["level"] != "WARN" || rec["run"] != float64(runID) || rec["job"] != "late" || rec["status"] != "succeeded" {
			t.Errorf("discard line = %v, want WARN run=%d job=late status=succeeded", rec, runID)
		}
		if errs := logs.atLevel(t, "ERROR"); len(errs) != 0 {
			t.Errorf("a discarded result is not an error, but logged: %v", errs)
		}
	})
}

// claimRunOf creates job, enqueues one run of it and claims that run the way a
// worker process would, returning the run ID. The run is left 'running' with
// nobody executing it. It must be the only pending run in the store.
func claimRunOf(t *testing.T, ctx context.Context, s store.Store, job jobs.Job) int64 {
	t.Helper()
	jobID, err := s.CreateJob(ctx, job)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	runID, err := s.EnqueueRun(ctx, job.OrgID, jobID)
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	run, ok, err := s.ClaimRun(ctx, "runner")
	if err != nil || !ok || run.ID != runID {
		t.Fatalf("ClaimRun: run=%d ok=%v err=%v, want run %d", run.ID, ok, err, runID)
	}
	return runID
}

// noBackoffExecutor returns an executor that retries without pausing.
func noBackoffExecutor() *jobs.Executor {
	ex := jobs.NewExecutor()
	ex.Backoff = func(int) time.Duration { return 0 }
	return ex
}
