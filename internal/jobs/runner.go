// Package jobs - runner.go wires the engine end-to-end: the scheduler tick
// enqueues due jobs, worker goroutines claim pending runs, the executor runs
// them with injected secrets, output is redacted, the terminal state is
// recorded, and lifecycle events are emitted. Startup reconciliation re-queues
// runs orphaned by a crash.
//
// IMPORT-CYCLE NOTE: the store package imports this (jobs) package, so this
// package MUST NOT import store. The runner depends on persistence through the
// consumer-defined RunnerStore interface below, which the concrete store types
// satisfy structurally.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/secrets"
)

// workerID is the constant claimant name stamped on runs. This single-instance
// runner has no peer workers, so a fixed identifier is sufficient.
const workerID = "runner"

// defaultTickInterval is the scheduler poll cadence when Runner.TickInterval is
// left at its zero value.
const defaultTickInterval = 10 * time.Second

// defaultWorkers is the number of draining goroutines when Runner.Workers is
// left unset (< 1).
const defaultWorkers = 2

// RunnerStore is the persistence surface the runner needs. It is defined here
// (in package jobs) rather than imported from store to avoid an import cycle:
// store imports jobs. The concrete *store.SQLiteStore / *store.PostgresStore
// satisfy this structurally.
type RunnerStore interface {
	DueJobs(ctx context.Context, now time.Time) ([]Job, error)
	EnqueueRun(ctx context.Context, orgID, jobID int64) (int64, error)
	UpdateJob(ctx context.Context, j Job) error
	GetJob(ctx context.Context, orgID, id int64) (Job, error)
	ClaimRun(ctx context.Context, worker string) (Run, bool, error)
	FinishRun(ctx context.Context, runID int64, status RunStatus, exitCode int, output string) error
	// FinishRunAndEmit atomically records the terminal run state (including the
	// final attempt count) AND the terminal lifecycle event in one transaction
	// (prevents lost terminal event on EmitEvent failure after FinishRun
	// committed). Returns the new event ID.
	FinishRunAndEmit(ctx context.Context, runID int64, status RunStatus, exitCode, attempt int, output string, ev core.Event) (int64, error)
	GetSecret(ctx context.Context, orgID int64, name string) (string, error)
	EmitEvent(ctx context.Context, e core.Event) (int64, error)
	RequeueOrphanedRuns(ctx context.Context) (int64, error)
}

// Runner ties the scheduler, run queue, executor, and event pipeline together.
type Runner struct {
	store RunnerStore
	exec  *Executor
	box   *secrets.Box // may be nil when no master key is configured
	clk   clock.Clock

	// TickInterval is the scheduler poll cadence; defaults to 10s when zero.
	TickInterval time.Duration
	// Workers is the number of draining goroutines; defaults to 2 when < 1.
	Workers int

	// EventSink, when non-nil, is invoked with each terminal run event
	// (run.succeeded / run.failed) AFTER it has been durably recorded by
	// FinishRunAndEmit. It is the seam through which the notify dispatcher
	// alerts on failures. It is nil-safe (see fire) and a settable field like
	// TickInterval/Workers - not a NewRunner argument.
	//
	// The runner fires terminal events UNCONDITIONALLY (both successes and
	// failures): the dispatcher's own `alertable` filter decides which event
	// types actually trigger a notification (run.succeeded is dropped), so the
	// runner does not need to know the alerting policy. The best-effort
	// run.started event is NOT a terminal event and is never fired here.
	//
	// jobs deliberately keeps this a plain func over core.Event (not a notify
	// type) so package jobs never imports notify - avoiding an import cycle.
	EventSink func(context.Context, core.Event)
}

// fire invokes EventSink with ev when a sink is configured. It is nil-safe so
// callers (and tests that leave EventSink unset) need no guard.
func (r *Runner) fire(ctx context.Context, ev core.Event) {
	if r.EventSink != nil {
		r.EventSink(ctx, ev)
	}
}

// NewRunner constructs a Runner. box may be nil when no secrets are configured;
// jobs that reference a secret will then fail at resolve time (recorded as a
// failed run, never executed).
func NewRunner(s RunnerStore, ex *Executor, box *secrets.Box, clk clock.Clock) *Runner {
	return &Runner{store: s, exec: ex, box: box, clk: clk}
}

// secretRefRe matches a value that is exactly a secret reference of the form
// {{ secret.NAME }} (surrounding whitespace optional). NAME is captured.
var secretRefRe = regexp.MustCompile(`^\{\{\s*secret\.([A-Za-z0-9_.-]+)\s*\}\}$`)

// Tick enqueues a run for every due job and advances each job's schedule.
//
// SCHEDULING CONTRACT: the runner only fires jobs whose NextRunAt is set and
// <= now (exactly what DueJobs returns). Computing a job's INITIAL NextRunAt
// when it is first created/enabled is the job-creation path's responsibility
// (the CLI/config sync in Task 9), NOT the runner's. The runner only ADVANCES
// NextRunAt after a fire.
//
// Per-job errors do not abort the pass: every due job is attempted and the
// first error encountered is returned afterward.
func (r *Runner) Tick(ctx context.Context) error {
	now := r.clk.Now()
	due, err := r.store.DueJobs(ctx, now)
	if err != nil {
		return err
	}

	var firstErr error
	record := func(e error) {
		if e != nil && firstErr == nil {
			firstErr = e
		}
	}

	for _, job := range due {
		if _, err := r.store.EnqueueRun(ctx, job.OrgID, job.ID); err != nil {
			record(err)
			continue
		}
		// Advance the schedule. A zero next time (elapsed one-off) is correct:
		// the job will simply not be due again.
		next, err := job.NextRun(now)
		if err != nil {
			// NextRun should not fail for a job DueJobs returned; this indicates a
			// corrupted or missing schedule expression. Clear NextRunAt and persist
			// it so DueJobs stops returning this job every tick (spin guard),
			// and surface the error so it shows up in Tick's return value.
			record(err)
			job.NextRunAt = time.Time{}
			if uerr := r.store.UpdateJob(ctx, job); uerr != nil {
				record(uerr)
			}
			continue
		}
		job.NextRunAt = next
		if err := r.store.UpdateJob(ctx, job); err != nil {
			record(err)
		}
	}
	return firstErr
}

// DrainOnce claims and runs every currently-pending run in a single pass,
// returning once the queue is empty.
func (r *Runner) DrainOnce(ctx context.Context) error {
	for {
		claimed, err := r.claimAndRun(ctx)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
	}
}

// claimAndRun claims one pending run and executes it end-to-end. It returns
// claimed=false (with no error) when the queue is empty. A run that fails
// secret resolution is recorded as failed and never executed; claimAndRun still
// returns claimed=true so draining continues.
//
// Recovery note: if claimAndRun returns an error after ClaimRun succeeds, the
// run is left in 'running'. Single-instance v1 recovers these at the next
// restart via RequeueOrphanedRuns (called by Start). There is NO in-process
// periodic reaper - a run orphaned mid-claim blocks that job's no-overlap guard
// until restart. M2 hardening: add a periodic reconcile that re-queues/fails
// 'running' rows whose started_at exceeds the job timeout.
func (r *Runner) claimAndRun(ctx context.Context) (bool, error) {
	run, ok, err := r.store.ClaimRun(ctx, workerID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	job, err := r.store.GetJob(ctx, run.OrgID, run.JobID)
	if err != nil {
		// Run is left in 'running'; recovered at next restart via RequeueOrphanedRuns.
		return true, err
	}

	// run.started is best-effort: a lost start event is non-critical because the
	// terminal event (run.succeeded / run.failed) is guaranteed atomic via
	// FinishRunAndEmit. Consumers that need to detect a missing start event can
	// infer it from the terminal event's run_id.
	if err := r.emit(ctx, run, job, "run.started", StatusRunning, 0); err != nil {
		// Non-fatal: continue to execution. The started event is advisory; the
		// terminal event written by FinishRunAndEmit is what pipeline consumers
		// rely on.
		_ = err
	}

	env, secretValues, err := r.resolveEnv(ctx, job)
	if err != nil {
		// Never execute with unresolved secrets. Record a failed run; the error
		// is sanitised by resolveEnv (no secret material). Finish + emit atomically
		// so the run.failed event is never lost.
		out := "secret resolution failed: " + err.Error()
		termEv, evErr := r.buildEvent(run, job, "run.failed", StatusFailed, -1)
		if evErr != nil {
			// Run is left in 'running'; recovered at next restart.
			return true, evErr
		}
		if _, ferr := r.store.FinishRunAndEmit(ctx, run.ID, StatusFailed, -1, run.Attempt, out, termEv); ferr != nil {
			// Run may be left in 'running'; recovered at next restart.
			return true, ferr
		}
		// Terminal event durably recorded - fire the sink (nil-safe).
		r.fire(ctx, termEv)
		return true, nil
	}

	res := r.exec.Run(ctx, job, env)
	out := redact(res.Output, secretValues)

	// I1: Map terminal event type to the documented vocabulary:
	//   run.succeeded  - when execution succeeded
	//   run.failed     - for ALL other terminal states (failed, timed_out, …)
	// The precise status is preserved in the payload's "status" field so
	// consumers can distinguish timed_out from failed without breaking the
	// type vocabulary.
	termType := "run.failed"
	if res.Status == StatusSucceeded {
		termType = "run.succeeded"
	}

	termEv, err := r.buildEvent(run, job, termType, res.Status, res.ExitCode)
	if err != nil {
		// Run is left in 'running'; recovered at next restart.
		return true, err
	}

	// I2: finish + terminal event in one atomic transaction - no lost terminal event.
	if _, err := r.store.FinishRunAndEmit(ctx, run.ID, res.Status, res.ExitCode, res.Attempt, out, termEv); err != nil {
		// Run is left in 'running'; recovered at next restart.
		return true, err
	}
	// Terminal event durably recorded - fire the sink (nil-safe). The dispatcher's
	// alertable filter drops run.succeeded, so firing unconditionally is correct.
	r.fire(ctx, termEv)
	return true, nil
}

// resolveEnv builds the execution environment for a job, decrypting any value
// that is a {{ secret.NAME }} reference. It returns the resolved env map and the
// slice of plaintext secret values (for output redaction). Errors never include
// secret material.
//
// M3: only a value that is EXACTLY "{{ secret.NAME }}" (full value, optional
// surrounding whitespace) is resolved. Partial templates such as
// "Bearer {{ secret.x }}" are treated as literals - they are NOT resolved and
// NOT redacted from output.
func (r *Runner) resolveEnv(ctx context.Context, job Job) (map[string]string, []string, error) {
	result := make(map[string]string, len(job.Env))
	var secretValues []string

	for key, value := range job.Env {
		m := secretRefRe.FindStringSubmatch(strings.TrimSpace(value))
		if m == nil {
			result[key] = value
			continue
		}
		name := m[1]
		if r.box == nil {
			return nil, nil, fmt.Errorf("secret %q referenced but no master key configured", name)
		}
		ciphertext, err := r.store.GetSecret(ctx, job.OrgID, name)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve secret %q: %w", name, err)
		}
		plain, err := r.box.Decrypt(ciphertext)
		if err != nil {
			return nil, nil, fmt.Errorf("decrypt secret %q: %w", name, err)
		}
		result[key] = string(plain)
		secretValues = append(secretValues, string(plain))
	}
	return result, secretValues, nil
}

// redact replaces every non-empty secret value in s with "***". This is a
// literal substring replacement; it will NOT catch secret values the job
// re-encodes (base64, URL-encode, JSON) before printing - accepted v1 limitation.
func redact(s string, secretValues []string) string {
	for _, v := range secretValues {
		if v == "" {
			continue
		}
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}

// buildEvent constructs a run.* lifecycle core.Event with the standard small
// payload (no secret material, no output). It is shared by emit (best-effort
// start event) and the terminal path in claimAndRun (atomic finish+emit).
func (r *Runner) buildEvent(run Run, job Job, typ string, status RunStatus, exitCode int) (core.Event, error) {
	payload, err := json.Marshal(struct {
		RunID    int64  `json:"run_id"`
		JobID    int64  `json:"job_id"`
		JobName  string `json:"job_name"`
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}{
		RunID:    run.ID,
		JobID:    run.JobID,
		JobName:  job.Name,
		Status:   string(status),
		ExitCode: exitCode,
	})
	if err != nil {
		return core.Event{}, fmt.Errorf("marshal event payload: %w", err)
	}
	return core.Event{
		OrgID:    run.OrgID,
		Type:     typ,
		Source:   "jobs.runner",
		Payload:  string(payload),
		DedupKey: fmt.Sprintf("run:%d:%s", run.ID, typ),
	}, nil
}

// emit appends a run.* lifecycle event. Payloads are deliberately small and
// contain NO secret material and NO (redacted) output - output lives in
// job_runs.
func (r *Runner) emit(ctx context.Context, run Run, job Job, typ string, status RunStatus, exitCode int) error {
	ev, err := r.buildEvent(run, job, typ, status, exitCode)
	if err != nil {
		return err
	}
	_, err = r.store.EmitEvent(ctx, ev)
	return err
}

// Start runs the autonomous loop until ctx is cancelled. It first reconciles
// crash-orphaned runs, then runs a scheduler ticker plus a pool of draining
// workers. It returns once every goroutine has stopped; a clean cancellation
// returns nil.
//
// I3: crash/DB-orphaned 'running' runs are recovered at startup via
// RequeueOrphanedRuns (at-least-once). There is NO in-process periodic reaper
// - a run left 'running' by a transient mid-claim failure is recovered only on
// the next restart; DueJobs' no-overlap guard means that job won't re-fire
// until then. M2 hardening: add a periodic reconcile that re-queues/fails
// 'running' rows whose started_at exceeds the job timeout.
func (r *Runner) Start(ctx context.Context) error {
	// 1. Reconcile crash-orphaned 'running' runs back to 'pending'.
	if _, err := r.store.RequeueOrphanedRuns(ctx); err != nil {
		return err
	}

	tick := r.TickInterval
	if tick <= 0 {
		tick = defaultTickInterval
	}
	workers := r.Workers
	if workers < 1 {
		workers = defaultWorkers
	}
	// Poll cadence for idle workers: short, but never longer than a tick.
	poll := tick
	if poll > time.Second {
		poll = time.Second
	}

	var wg sync.WaitGroup

	// Scheduler ticker.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Tick errors are transient (DB hiccups); the next tick retries.
				_ = r.Tick(ctx)
			}
		}
	}()

	// Draining workers.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				claimed, err := r.claimAndRun(ctx)
				if err != nil {
					// Transient store/exec error: back off one poll interval
					// rather than spin.
					if sleep(ctx, poll) {
						return
					}
					continue
				}
				if claimed {
					continue // drain fast: immediately try the next run
				}
				// Queue empty: wait a poll interval or until shutdown.
				if sleep(ctx, poll) {
					return
				}
			}
		}()
	}

	wg.Wait()
	if err := ctx.Err(); err != nil && err != context.Canceled {
		return err
	}
	return nil
}

// sleep waits for d or until ctx is done. It returns true if ctx was cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}
