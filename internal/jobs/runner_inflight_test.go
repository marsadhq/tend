package jobs_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/secrets"
	"github.com/marsadhq/tend/internal/store"
)

// blockingRun creates a job that keeps executing until the returned release
// function is called and then prints "done", enqueues one run of it, and
// returns the job and run IDs.
func blockingRun(t *testing.T, ctx context.Context, s store.Store, job jobs.Job) (jobID, runID int64, release func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "release")
	job.Type = jobs.Shell
	job.Command = "while [ ! -e '" + gate + "' ]; do sleep 0.05; done; echo done"
	job.Enabled = true
	jobID, err := s.CreateJob(ctx, job)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	runID, err = s.EnqueueRun(ctx, job.OrgID, jobID)
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	return jobID, runID, func() {
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatalf("release the job: %v", err)
		}
	}
}

// drainInBackground runs r.DrainOnce on its own goroutine and waits until r
// has registered runID as in flight, so the caller knows the job is executing.
func drainInBackground(t *testing.T, ctx context.Context, r *jobs.Runner, runID int64) <-chan error {
	t.Helper()
	drained := make(chan error, 1)
	go func() { drained <- r.DrainOnce(ctx) }()
	waitFor(t, fmt.Sprintf("run %d to be in flight", runID), func() bool {
		return slices.Equal(r.InFlightRuns(), []int64{runID})
	})
	return drained
}

// editJob applies change to the stored job, the way tend sync or a job edit
// rewrites a definition while a run of it may be in flight.
func editJob(t *testing.T, ctx context.Context, s store.Store, orgID, jobID int64, change func(*jobs.Job)) {
	t.Helper()
	job, err := s.GetJob(ctx, orgID, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	change(&job)
	if err := s.UpdateJob(ctx, job); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
}

// mustGetRun returns the stored run.
func mustGetRun(t *testing.T, ctx context.Context, s store.Store, orgID, runID int64) jobs.Run {
	t.Helper()
	run, err := s.GetRun(ctx, orgID, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return run
}

// TestReaperKeepsAnInFlightRunWhenItsJobIsEdited proves the reaper judges a
// run this process is executing by the definition it was claimed with. The job
// is claimed with a two hour timeout and two retries; while it executes, its
// timeout is cut to one second and its retries to none. Sweeps an hour in, and
// again just short of the claim-time deadline, must leave the run alone, and
// the job's real result must be what is recorded.
func TestReaperKeepsAnInFlightRunWhenItsJobIsEdited(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		jobID, runID, release := blockingRun(t, ctx, s, jobs.Job{
			OrgID: orgID, Name: "long", TimeoutSeconds: 7200, MaxRetries: 2,
		})

		fk := clock.NewFake(time.Now())
		sink := &countingSink{}
		logger, logs := newLogCapture()
		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk)
		r.EventSink = sink.fire
		r.Logger = logger
		drained := drainInBackground(t, ctx, r, runID)

		editJob(t, ctx, s, orgID, jobID, func(j *jobs.Job) { j.TimeoutSeconds, j.MaxRetries = 1, 0 })

		// Claimed with: 3 attempts of 2h plus 5s kill grace, pauses of 1s and 4s,
		// and 2m of slack on top = 6h2m20s. The edited job would allow 2m6s.
		const claimTimeDeadline = 3*(2*time.Hour+5*time.Second) + 5*time.Second + 2*time.Minute
		// Sweep one hour in, then one minute short of the claim-time deadline.
		fk.Advance(time.Hour)
		if err := r.ReapOnce(ctx); err != nil {
			t.Fatalf("ReapOnce after 1h: %v", err)
		}
		fk.Advance(claimTimeDeadline - time.Minute - time.Hour)
		if err := r.ReapOnce(ctx); err != nil {
			t.Fatalf("ReapOnce before the claim-time deadline: %v", err)
		}
		if run := mustGetRun(t, ctx, s, orgID, runID); run.Status != jobs.StatusRunning {
			t.Fatalf("run status = %s %q, want running: a healthy run was reaped on the edited definition", run.Status, run.Output)
		}
		if counts := eventCounts(t, ctx, s, orgID); counts["run.failed"] != 0 {
			t.Fatalf("events = %v, want no run.failed", counts)
		}
		if got := sink.types(); len(got) != 0 {
			t.Fatalf("sink received %v, want nothing", got)
		}

		release()
		if err := <-drained; err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		run := mustGetRun(t, ctx, s, orgID, runID)
		if run.Status != jobs.StatusSucceeded || run.Output != "done\n" {
			t.Errorf("run = %s %q, want the job's real result", run.Status, run.Output)
		}
		counts := eventCounts(t, ctx, s, orgID)
		if counts["run.succeeded"] != 1 || counts["run.failed"] != 0 {
			t.Errorf("events = %v, want exactly one run.succeeded", counts)
		}
		if got := sink.types(); len(got) != 1 || got[0] != "run.succeeded" {
			t.Errorf("sink received %v, want one run.succeeded", got)
		}
		if recs := logs.atLevel(t, "WARN"); len(recs) != 0 {
			t.Errorf("logged warnings for a healthy run: %v", recs)
		}
		if ids := r.InFlightRuns(); len(ids) != 0 {
			t.Errorf("runs still registered after the worker returned: %v", ids)
		}
	})
}

// TestReaperFailsAnInFlightRunPastItsClaimTimeLimit proves the claim-time
// deadline is a deadline, not an exemption, and that it does not move with the
// job either way: the job is claimed with a ten minute timeout, which is then
// raised to a day. Twenty minutes in, the run is past the limit it was claimed
// with, so the reaper fails it. When its worker returns after all, the late
// result is discarded and that is logged.
func TestReaperFailsAnInFlightRunPastItsClaimTimeLimit(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		jobID, runID, release := blockingRun(t, ctx, s, jobs.Job{
			OrgID: orgID, Name: "stuck", TimeoutSeconds: 600,
		})

		fk := clock.NewFake(time.Now())
		sink := &countingSink{}
		logger, logs := newLogCapture()
		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk)
		r.EventSink = sink.fire
		r.Logger = logger
		drained := drainInBackground(t, ctx, r, runID)

		editJob(t, ctx, s, orgID, jobID, func(j *jobs.Job) { j.TimeoutSeconds = 86400 })

		// Inside the claim-time window (10m5s plus 2m of slack): left alone.
		fk.Advance(11 * time.Minute)
		if err := r.ReapOnce(ctx); err != nil {
			t.Fatalf("ReapOnce after 11m: %v", err)
		}
		if run := mustGetRun(t, ctx, s, orgID, runID); run.Status != jobs.StatusRunning {
			t.Fatalf("run status after 11m = %s, want running", run.Status)
		}

		// Past it: reaped, although the job as edited would allow a day.
		fk.Advance(9 * time.Minute)
		if err := r.ReapOnce(ctx); err != nil {
			t.Fatalf("ReapOnce after 20m: %v", err)
		}
		const wantOutput = "reaped: run still 'running' 2m0s past its 10m5s time limit (worker has not returned)"
		if run := mustGetRun(t, ctx, s, orgID, runID); run.Status != jobs.StatusFailed || run.Output != wantOutput {
			t.Fatalf("run after 20m = %s %q, want failed with %q", run.Status, run.Output, wantOutput)
		}
		reaps := logs.withMsg(t, "runner: reaped stale run")
		if len(reaps) != 1 || reaps[0]["run"] != float64(runID) || reaps[0]["job"] != "stuck" {
			t.Errorf("reap log = %v, want one line for run %d of job stuck", reaps, runID)
		}
		if ids := r.InFlightRuns(); !slices.Equal(ids, []int64{runID}) {
			t.Errorf("in-flight runs = %v, want the run still registered while its worker is busy", ids)
		}

		// The worker comes back with a success for a run that is already failed.
		release()
		if err := <-drained; err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		if run := mustGetRun(t, ctx, s, orgID, runID); run.Status != jobs.StatusFailed || run.Output != wantOutput {
			t.Errorf("run after the late result = %s %q, want the reaped state untouched", run.Status, run.Output)
		}
		counts := eventCounts(t, ctx, s, orgID)
		if counts["run.failed"] != 1 || counts["run.succeeded"] != 0 {
			t.Errorf("events = %v, want exactly one run.failed", counts)
		}
		if got := sink.types(); len(got) != 1 || got[0] != "run.failed" {
			t.Errorf("sink received %v, want one run.failed", got)
		}
		discards := logs.withMsg(t, "runner: discarded late result of a run that is no longer running")
		if len(discards) != 1 || discards[0]["run"] != float64(runID) || discards[0]["job"] != "stuck" ||
			discards[0]["status"] != "succeeded" || discards[0]["level"] != "WARN" {
			t.Errorf("discard log = %v, want one WARN line for run %d of job stuck, status succeeded", discards, runID)
		}
		if ids := r.InFlightRuns(); len(ids) != 0 {
			t.Errorf("runs still registered after the worker returned: %v", ids)
		}
	})
}

// TestReaperJudgesAnotherProcessRunByTheCurrentDefinition proves a run that
// this process is not executing is reaped exactly as before: on the job as it
// is now. One runner stands in for the other process and executes the run; a
// second runner, whose registry therefore does not know the run, sweeps. An
// hour in it leaves the run alone while the job still says two hours, and fails
// it once the job has been edited down to a second.
func TestReaperJudgesAnotherProcessRunByTheCurrentDefinition(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		jobID, runID, release := blockingRun(t, ctx, s, jobs.Job{
			OrgID: orgID, Name: "elsewhere", TimeoutSeconds: 7200,
		})

		otherLogger, otherLogs := newLogCapture()
		other := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.RealClock{})
		other.Logger = otherLogger
		drained := drainInBackground(t, ctx, other, runID)

		fk := clock.NewFake(time.Now().Add(time.Hour))
		sink := &countingSink{}
		reaper := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk)
		reaper.EventSink = sink.fire
		if ids := reaper.InFlightRuns(); len(ids) != 0 {
			t.Fatalf("the sweeping runner has runs in flight: %v", ids)
		}

		if err := reaper.ReapOnce(ctx); err != nil {
			t.Fatalf("ReapOnce: %v", err)
		}
		if run := mustGetRun(t, ctx, s, orgID, runID); run.Status != jobs.StatusRunning {
			t.Fatalf("run status = %s, want running: one hour into a two hour timeout", run.Status)
		}

		editJob(t, ctx, s, orgID, jobID, func(j *jobs.Job) { j.TimeoutSeconds = 1 })
		if err := reaper.ReapOnce(ctx); err != nil {
			t.Fatalf("ReapOnce after the edit: %v", err)
		}
		const wantOutput = "reaped: run still 'running' 2m0s past its 6s time limit (worker lost)"
		if run := mustGetRun(t, ctx, s, orgID, runID); run.Status != jobs.StatusFailed || run.Output != wantOutput {
			t.Fatalf("run = %s %q, want failed with %q", run.Status, run.Output, wantOutput)
		}
		if got := sink.types(); len(got) != 1 || got[0] != "run.failed" {
			t.Errorf("sink received %v, want one run.failed", got)
		}

		// The other process finishes: its result is late and it says so.
		release()
		if err := <-drained; err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		if run := mustGetRun(t, ctx, s, orgID, runID); run.Status != jobs.StatusFailed || run.Output != wantOutput {
			t.Errorf("run after the late result = %s %q, want the reaped state untouched", run.Status, run.Output)
		}
		if counts := eventCounts(t, ctx, s, orgID); counts["run.failed"] != 1 || counts["run.succeeded"] != 0 {
			t.Errorf("events = %v, want exactly one run.failed", counts)
		}
		if recs := otherLogs.withMsg(t, "runner: discarded late result of a run that is no longer running"); len(recs) != 1 {
			t.Errorf("the executing runner logged %d discards, want 1: %v", len(recs), otherLogs.records(t))
		}
	})
}

// TestReaperJudgesAnOrphanedRunByTheCurrentDefinition proves the same for a
// run nobody is executing at all (its worker is gone): the job's current
// definition decides, so shortening the timeout shortens the wait.
func TestReaperJudgesAnOrphanedRunByTheCurrentDefinition(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		runID := claimRunOf(t, ctx, s, jobs.Job{
			OrgID: orgID, Name: "orphan", Type: jobs.Shell, Command: "sleep 999", TimeoutSeconds: 7200, Enabled: true,
		})
		run := mustGetRun(t, ctx, s, orgID, runID)

		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.NewFake(time.Now().Add(time.Hour)))
		if err := r.ReapOnce(ctx); err != nil {
			t.Fatalf("ReapOnce: %v", err)
		}
		if got := mustGetRun(t, ctx, s, orgID, runID); got.Status != jobs.StatusRunning {
			t.Fatalf("run status = %s, want running: one hour into a two hour timeout", got.Status)
		}

		editJob(t, ctx, s, orgID, run.JobID, func(j *jobs.Job) { j.TimeoutSeconds = 1 })
		if err := r.ReapOnce(ctx); err != nil {
			t.Fatalf("ReapOnce after the edit: %v", err)
		}
		got := mustGetRun(t, ctx, s, orgID, runID)
		if got.Status != jobs.StatusFailed || !strings.Contains(got.Output, "6s time limit (worker lost)") {
			t.Errorf("run = %s %q, want failed as worker lost on the 6s limit", got.Status, got.Output)
		}
	})
}

// TestInFlightRegistryIsEmptiedHoweverARunEnds proves the registry cannot leak
// an entry: whichever way a claimed run ends - finished, failed, timed out,
// never executed, unrecorded, reaped, cancelled, or with a panic unwinding the
// worker - the run is registered while it is being handled and gone afterwards.
func TestInFlightRegistryIsEmptiedHoweverARunEnds(t *testing.T) {
	boom := errors.New("store is unavailable")
	shell := func(name, command string) jobs.Job {
		return jobs.Job{Name: name, Type: jobs.Shell, Command: command, Enabled: true}
	}

	cases := []struct {
		name    string
		job     jobs.Job
		arrange func(fs *faultStore, r *jobs.Runner, cancel context.CancelFunc)
		wantErr bool
		// wantPanic is the value DrainOnce is expected to panic with.
		wantPanic any
		// neverFinishes: the run does not reach the store's finish call.
		neverFinishes bool
	}{
		{name: "succeeded", job: shell("ok", "echo ok")},
		{name: "failed", job: shell("fails", "exit 3")},
		{
			name: "timed out",
			job: func() jobs.Job {
				j := shell("slow", "sleep 30")
				j.TimeoutSeconds = 1
				return j
			}(),
		},
		{
			name: "secret not resolved",
			job: func() jobs.Job {
				j := shell("no-secret", "echo $X")
				j.Env = map[string]string{"X": "{{ secret.missing }}"}
				return j
			}(),
		},
		{
			name: "result could not be recorded",
			job:  shell("unrecorded", "echo ok"),
			arrange: func(fs *faultStore, _ *jobs.Runner, _ context.CancelFunc) {
				fs.onFinish = func(int64) error { return boom }
			},
			wantErr: true,
		},
		{
			name: "reaped while it was executing",
			job:  shell("reaped", "echo ok"),
			arrange: func(fs *faultStore, _ *jobs.Runner, _ context.CancelFunc) {
				fs.onFinish = func(int64) error { return jobs.ErrRunNotRunning }
			},
		},
		{
			name: "store panics while finishing",
			job:  shell("store-panic", "echo ok"),
			arrange: func(fs *faultStore, _ *jobs.Runner, _ context.CancelFunc) {
				fs.onFinish = func(int64) error { panic("finish blew up") }
			},
			wantPanic: "finish blew up",
		},
		{
			name: "sink panics after the finish",
			job:  shell("sink-panic", "echo ok"),
			arrange: func(_ *faultStore, r *jobs.Runner, _ context.CancelFunc) {
				r.EventSink = func(context.Context, core.Event) { panic("sink blew up") }
			},
			wantPanic: "sink blew up",
		},
		{
			name: "cancelled while it was executing",
			job:  shell("cancelled", "sleep 30"),
			arrange: func(_ *faultStore, r *jobs.Runner, cancel context.CancelFunc) {
				go func() {
					for len(r.InFlightRuns()) == 0 {
						time.Sleep(5 * time.Millisecond)
					}
					cancel()
				}()
			},
			wantErr: true,
		},
		{
			name: "job could not be loaded",
			job:  shell("unloadable", "echo ok"),
			arrange: func(fs *faultStore, _ *jobs.Runner, _ context.CancelFunc) {
				fs.onGetJob = func() error { return boom }
			},
			wantErr:       true,
			neverFinishes: true,
		},
	}

	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachStore(t, func(t *testing.T, s store.Store) {
				setupCtx := context.Background()
				job := tc.job
				job.OrgID = seedOrg(t, setupCtx, s)
				jobID, err := s.CreateJob(setupCtx, job)
				if err != nil {
					t.Fatal(err)
				}
				runID, err := s.EnqueueRun(setupCtx, job.OrgID, jobID)
				if err != nil {
					t.Fatal(err)
				}

				ctx, cancel := context.WithCancel(setupCtx)
				defer cancel()
				fs := &faultStore{Store: s}
				r := jobs.NewRunner(fs, jobs.NewExecutor(), box, clock.RealClock{})
				r.FinishBackoff = func(int) time.Duration { return 0 }
				if tc.arrange != nil {
					tc.arrange(fs, r, cancel)
				}
				// Whatever the case does at the finish, first note whether the run
				// is registered at that moment.
				var finishes int
				var registeredAtFinish bool
				inner := fs.onFinish
				fs.onFinish = func(id int64) error {
					finishes++
					registeredAtFinish = slices.Contains(r.InFlightRuns(), id)
					if inner != nil {
						return inner(id)
					}
					return nil
				}

				var drainErr error
				panicked := func() (p any) {
					defer func() { p = recover() }()
					drainErr = r.DrainOnce(ctx)
					return nil
				}()

				if panicked != tc.wantPanic {
					t.Fatalf("DrainOnce panicked with %v, want %v", panicked, tc.wantPanic)
				}
				if (drainErr != nil) != tc.wantErr {
					t.Errorf("DrainOnce error = %v, want an error: %v", drainErr, tc.wantErr)
				}
				if tc.neverFinishes {
					if finishes != 0 {
						t.Errorf("finish was attempted %d times, want none", finishes)
					}
				} else if finishes == 0 || !registeredAtFinish {
					t.Errorf("at the finish: attempts=%d registered=%v, want the run %d registered while it is handled",
						finishes, registeredAtFinish, runID)
				}
				if ids := r.InFlightRuns(); len(ids) != 0 {
					t.Errorf("registry still holds %v after the run ended", ids)
				}
			})
		})
	}
}

// TestWorkerAndReaperRaceLeavesOneTerminalStatePerRun races workers against
// the reaper for real. The runner's clock is a day ahead, so every run is past
// its deadline the moment it is claimed and the reaper tries to fail it while
// its worker is finishing it. Whoever wins, each run must end with exactly one
// terminal state, exactly one terminal event of the matching type, and exactly
// one sink call; nothing may be left running or registered.
func TestWorkerAndReaperRaceLeavesOneTerminalStatePerRun(t *testing.T) {
	const runs = 24
	const workers = 4

	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)

		runIDs := make([]int64, 0, runs)
		for i := 0; i < runs; i++ {
			jobID, err := s.CreateJob(ctx, jobs.Job{
				OrgID: orgID, Name: fmt.Sprintf("racer-%02d", i), Type: jobs.Shell,
				Command: "echo raced", TimeoutSeconds: 1, Enabled: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			runID, err := s.EnqueueRun(ctx, orgID, jobID)
			if err != nil {
				t.Fatal(err)
			}
			runIDs = append(runIDs, runID)
		}

		var mu sync.Mutex
		fired := map[int64][]string{} // run ID -> terminal event types sent to the sink
		logger, logs := newLogCapture()
		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.NewFake(time.Now().Add(24*time.Hour)))
		r.Logger = logger
		r.EventSink = func(_ context.Context, ev core.Event) {
			mu.Lock()
			defer mu.Unlock()
			id := eventRunID(t, ev)
			fired[id] = append(fired[id], ev.Type)
		}

		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := r.DrainOnce(ctx); err != nil {
					t.Errorf("DrainOnce: %v", err)
				}
			}()
		}
		stop := make(chan struct{})
		reaperDone := make(chan struct{})
		go func() {
			defer close(reaperDone)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := r.ReapOnce(ctx); err != nil {
					t.Errorf("ReapOnce: %v", err)
				}
			}
		}()
		wg.Wait()
		close(stop)
		<-reaperDone

		if running, err := s.ListRunningRuns(ctx); err != nil || len(running) != 0 {
			t.Fatalf("still running after the race: %d runs, err %v", len(running), err)
		}
		if ids := r.InFlightRuns(); len(ids) != 0 {
			t.Errorf("registry still holds %v", ids)
		}

		stored := map[int64][]string{} // run ID -> terminal event types in the store
		evts, err := s.ListEvents(ctx, orgID, 10*runs)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		for _, e := range evts {
			if e.Type == "run.succeeded" || e.Type == "run.failed" {
				id := eventRunID(t, e)
				stored[id] = append(stored[id], e.Type)
			}
		}

		reaped := 0
		for _, id := range runIDs {
			run := mustGetRun(t, ctx, s, orgID, id)
			var want string
			switch run.Status {
			case jobs.StatusSucceeded:
				want = "run.succeeded"
				if run.Output != "raced\n" {
					t.Errorf("run %d succeeded with output %q, want the job's output", id, run.Output)
				}
			case jobs.StatusFailed:
				want = "run.failed"
				reaped++
				if !strings.HasPrefix(run.Output, "reaped: ") {
					t.Errorf("run %d failed with output %q, want the reaper's", id, run.Output)
				}
			default:
				t.Errorf("run %d ended as %s, want a terminal state", id, run.Status)
				continue
			}
			if got := stored[id]; len(got) != 1 || got[0] != want {
				t.Errorf("run %d (%s): terminal events in the store = %v, want exactly [%s]", id, run.Status, got, want)
			}
			mu.Lock()
			got := fired[id]
			mu.Unlock()
			if len(got) != 1 || got[0] != want {
				t.Errorf("run %d (%s): sink received %v, want exactly [%s]", id, run.Status, got, want)
			}
		}
		// Every reap is logged, and so is the result each reap made late.
		if n := len(logs.withMsg(t, "runner: reaped stale run")); n != reaped {
			t.Errorf("logged %d reaps, want %d", n, reaped)
		}
		if n := len(logs.withMsg(t, "runner: discarded late result of a run that is no longer running")); n != reaped {
			t.Errorf("logged %d discarded results, want %d", n, reaped)
		}
		t.Logf("%d of %d runs were reaped before their worker finished", reaped, runs)
	})
}

// eventRunID returns the run_id in a run.* event's payload.
func eventRunID(t *testing.T, ev core.Event) int64 {
	var p struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil || p.RunID == 0 {
		t.Errorf("event %s has no run_id in its payload %q (%v)", ev.Type, ev.Payload, err)
	}
	return p.RunID
}
