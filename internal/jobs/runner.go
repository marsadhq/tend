// Package jobs - runner.go wires the engine end-to-end: the scheduler tick
// enqueues due jobs, worker goroutines claim pending runs, the executor runs
// them with injected secrets, output is redacted, the terminal state is
// recorded, and lifecycle events are emitted. Startup reconciliation re-queues
// runs orphaned by a crash. What goes wrong along the way is logged through
// Runner.Logger.
//
// IMPORT-CYCLE NOTE: the store package imports this (jobs) package, so this
// package MUST NOT import store. The runner depends on persistence through the
// consumer-defined RunnerStore interface below, which the concrete store types
// satisfy structurally.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// reapInterval is the cadence of the periodic stale-run sweep run by Start.
const reapInterval = time.Minute

// reapSlack is the grace added beyond the longest a run can legitimately take
// (Executor.maxRunDuration: every attempt timing out, plus the backoff between
// retries) before a 'running' run is considered orphaned. The executor kills
// each attempt at the timeout, so a run still 'running' this far past that
// limit lost its worker (crash/exit mid-run) and would otherwise block the
// job's no-overlap guard until restart.
const reapSlack = 2 * time.Minute

// ErrRunNotRunning is returned by RunnerStore.FinishRun and FinishRunAndEmit
// when the run exists but is no longer 'running', so nothing was written. In
// practice the stale-run reaper got there first: it already recorded the run
// as failed and emitted its terminal event, and that outcome stands.
var ErrRunNotRunning = errors.New("run is not running")

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
	// FinishRun records the terminal state of a 'running' run; it returns
	// ErrRunNotRunning, writing nothing, when the run is no longer 'running'.
	FinishRun(ctx context.Context, runID int64, status RunStatus, exitCode int, output string) error
	// FinishRunAndEmit atomically records the terminal run state (including the
	// final attempt count) AND the terminal lifecycle event in one transaction
	// (prevents lost terminal event on EmitEvent failure after FinishRun
	// committed). Returns the new event ID, or ErrRunNotRunning, having written
	// neither, when the run is no longer 'running'.
	FinishRunAndEmit(ctx context.Context, runID int64, status RunStatus, exitCode, attempt int, output string, ev core.Event) (int64, error)
	GetSecret(ctx context.Context, orgID int64, name string) (string, error)
	EmitEvent(ctx context.Context, e core.Event) (int64, error)
	RequeueOrphanedRuns(ctx context.Context) (int64, error)
	// ListRunningRuns returns every run currently in 'running' (all orgs), for
	// the periodic reaper sweep.
	ListRunningRuns(ctx context.Context) ([]Run, error)
	// ReapStaleRun atomically fails a run still in 'running' AND appends ev in
	// the same transaction, mirroring FinishRunAndEmit. It returns false (no
	// event written) when the run already reached a terminal state, closing the
	// race with a worker finishing the run concurrently.
	ReapStaleRun(ctx context.Context, runID int64, output string, ev core.Event) (bool, error)
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
	// FinishRunAndEmit or ReapStaleRun. serve uses it to nudge the notification
	// delivery worker; the deliveries themselves were already enqueued by the
	// store, in the same transaction as the event. It is nil-safe (see fire)
	// and a settable field like TickInterval/Workers - not a NewRunner argument.
	//
	// The runner fires terminal events UNCONDITIONALLY (both successes and
	// failures): the store decides at enqueue time, through notify.Alertable
	// and the notification rules, which events become deliveries
	// (run.succeeded never does), so the runner does not need to know the
	// alerting policy. The best-effort run.started event is NOT a terminal
	// event and is never fired here.
	//
	// jobs deliberately keeps this a plain func over core.Event (not a notify
	// type) so package jobs never imports notify - avoiding an import cycle.
	EventSink func(context.Context, core.Event)

	// Logger, when non-nil, receives the runner's log lines: every store error
	// from the scheduler tick, the reaper and the claim/finish path at ERROR
	// (a lost run.started event, which is only advisory, at WARN), each reaped
	// run and each discarded late result at WARN, and the number of runs
	// requeued at startup at INFO. Left nil, the runner logs nothing. Like the
	// fields above it is set before Start and not changed afterwards.
	Logger *slog.Logger

	// claimErrs tracks a claim that keeps failing. That is the one error not
	// logged every time it happens; see claimErrors.
	claimErrs claimErrors
}

// discardLogger backs a Runner whose Logger was left nil.
var discardLogger = slog.New(slog.DiscardHandler)

// log returns the configured logger, or one that discards everything, so call
// sites need no nil check.
func (r *Runner) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return discardLogger
}

// logError logs a failed step at ERROR with err and attrs. A step that failed
// while ctx was already cancelled is not logged: shutdown interrupts whatever
// store call is in flight, and that is expected, not an error to act on.
func (r *Runner) logError(ctx context.Context, msg string, err error, attrs ...any) {
	if ctx.Err() != nil {
		return
	}
	r.log().Error(msg, append(attrs, "err", err)...)
}

// claimErrorRepeat is how often a claim that keeps failing is logged again
// after its first failure; see claimErrors.
const claimErrorRepeat = time.Minute

// claimErrors limits how often a claim that keeps failing is logged. Each idle
// worker tries to claim a run about once a second, so a store that fails at
// once (a full disk, a database that refuses connections) would otherwise
// write an ERROR line per worker per second for as long as that lasts. The
// first failure is logged as it happens, the ones that follow at most once per
// claimErrorRepeat with the number of failed claims so far, and the first
// claim that works again ends the streak with a line of its own. The zero
// value is ready to use.
type claimErrors struct {
	mu      sync.Mutex
	failed  int       // failed claims in a row, over all workers
	lastLog time.Time // when a failure of this streak was last logged
}

// logClaimError logs a failed ClaimRun at ERROR, subject to claimErrors. Like
// logError it stays quiet about a claim that shutdown interrupted.
func (r *Runner) logClaimError(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	now := r.clk.Now()
	c := &r.claimErrs
	c.mu.Lock()
	c.failed++
	failed := c.failed
	due := failed == 1 || now.Sub(c.lastLog) >= claimErrorRepeat
	if due {
		c.lastLog = now
	}
	c.mu.Unlock()

	switch {
	case failed == 1:
		r.log().Error("runner: claim run", "err", err)
	case due:
		r.log().Error("runner: claim run is still failing", "failed_claims", failed, "err", err)
	}
}

// claimWorked ends a streak of failed claims, if there was one, and logs that
// it is over.
func (r *Runner) claimWorked() {
	c := &r.claimErrs
	c.mu.Lock()
	failed := c.failed
	c.failed = 0
	c.mu.Unlock()
	if failed > 0 {
		r.log().Info("runner: claim run works again", "failed_claims", failed)
	}
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
// Per-job errors do not abort the pass: every due job is attempted, each error
// is logged with the job it belongs to, and the first one encountered is
// returned afterward.
func (r *Runner) Tick(ctx context.Context) error {
	now := r.clk.Now()
	due, err := r.store.DueJobs(ctx, now)
	if err != nil {
		r.logError(ctx, "runner: scheduler tick: list due jobs", err)
		return err
	}

	var firstErr error
	record := func(job Job, step string, e error) {
		r.logError(ctx, "runner: scheduler tick: "+step, e, "job", job.Name)
		if firstErr == nil {
			firstErr = e
		}
	}

	for _, job := range due {
		if _, err := r.store.EnqueueRun(ctx, job.OrgID, job.ID); err != nil {
			record(job, "enqueue run", err)
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
			record(job, "compute next run", err)
			job.NextRunAt = time.Time{}
			if uerr := r.store.UpdateJob(ctx, job); uerr != nil {
				record(job, "clear schedule", uerr)
			}
			continue
		}
		job.NextRunAt = next
		if err := r.store.UpdateJob(ctx, job); err != nil {
			record(job, "advance schedule", err)
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
// run is left in 'running'. It is recovered either at the next restart via
// RequeueOrphanedRuns (called by Start) or by the periodic reaper (ReapOnce),
// which fails it once the run's time limit plus reapSlack has passed.
//
// Every error claimAndRun returns has already been logged here, where the run
// and the job are known, so callers must not log it a second time. (A claim
// that keeps failing is logged only every so often; see claimErrors.)
func (r *Runner) claimAndRun(ctx context.Context) (bool, error) {
	run, ok, err := r.store.ClaimRun(ctx, workerID)
	if err != nil {
		r.logClaimError(ctx, err)
		return false, err
	}
	r.claimWorked()
	if !ok {
		return false, nil
	}

	job, err := r.store.GetJob(ctx, run.OrgID, run.JobID)
	if err != nil {
		// Run is left in 'running'; recovered at next restart via RequeueOrphanedRuns.
		r.logError(ctx, "runner: load job of claimed run", err, "run", run.ID, "job_id", run.JobID)
		return true, err
	}

	// run.started is best-effort: a lost start event is non-critical because the
	// terminal event (run.succeeded / run.failed) is guaranteed atomic via
	// FinishRunAndEmit. Consumers that need to detect a missing start event can
	// infer it from the terminal event's run_id.
	if err := r.emit(ctx, run, job, "run.started", StatusRunning, 0); err != nil && ctx.Err() == nil {
		// Non-fatal: continue to execution. The started event is advisory; the
		// terminal event written by FinishRunAndEmit is what pipeline consumers
		// rely on. Worth a line all the same: it is the first sign of a store
		// that is refusing writes.
		r.log().Warn("runner: record run.started", "run", run.ID, "job", job.Name, "err", err)
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
			r.logError(ctx, "runner: build terminal event", evErr, "run", run.ID, "job", job.Name)
			return true, evErr
		}
		return true, r.finish(ctx, run, job, StatusFailed, -1, run.Attempt, out, termEv)
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
		r.logError(ctx, "runner: build terminal event", err, "run", run.ID, "job", job.Name)
		return true, err
	}
	return true, r.finish(ctx, run, job, res.Status, res.ExitCode, res.Attempt, out, termEv)
}

// finish records the terminal state of a claimed run together with its
// terminal event (I2: one atomic transaction, so the event is never lost) and
// then fires the sink. Both ways a run ends go through here: a result from the
// executor, and a secret that could not be resolved.
//
// A run that is no longer 'running' was failed by the reaper while its worker
// was still busy, and the reaper has already emitted its run.failed. A terminal
// state is final: the late result is dropped, with no second event and no sink
// call, and that is logged so the lost result is not silent. finish returns nil
// for it, since the worker has nothing left to do.
//
// Any other error leaves the run in 'running' (recovered at the next restart
// or by the reaper); it is logged with the run and its job and returned.
func (r *Runner) finish(ctx context.Context, run Run, job Job, status RunStatus, exitCode, attempt int, output string, termEv core.Event) error {
	_, err := r.store.FinishRunAndEmit(ctx, run.ID, status, exitCode, attempt, output, termEv)
	if errors.Is(err, ErrRunNotRunning) {
		r.log().Warn("runner: discarded late result of a run that is no longer running",
			"run", run.ID, "job", job.Name, "status", string(status), "exit_code", exitCode)
		return nil
	}
	if err != nil {
		r.logError(ctx, "runner: finish run", err,
			"run", run.ID, "job", job.Name, "status", string(status))
		return err
	}
	// Terminal event durably recorded - fire the sink (nil-safe). The store's
	// Alertable filter already kept run.succeeded out of the delivery queue, so
	// firing unconditionally is correct.
	r.fire(ctx, termEv)
	return nil
}

// ReapOnce fails every 'running' run whose deadline has passed, emitting
// run.failed for each so the orphan is neither silent nor left blocking the
// job's no-overlap guard until the next restart. The deadline is started_at +
// the longest the run can legitimately take + reapSlack. started_at is stamped
// once, when the run is claimed, while the executor may spend up to
// MaxRetries+1 attempts of the job's timeout each, with a backoff pause in
// between, so the limit covers all of them: a run that is still retrying is
// not an orphan. The store-side status guard in ReapStaleRun makes the sweep
// safe against a worker finishing the run concurrently. Per-run errors do not
// abort the sweep; each is logged with its run and the first one encountered is
// returned afterward. Every run that is reaped is logged too.
func (r *Runner) ReapOnce(ctx context.Context) error {
	runs, err := r.store.ListRunningRuns(ctx)
	if err != nil {
		r.logError(ctx, "runner: reaper: list running runs", err)
		return err
	}
	now := r.clk.Now()

	var firstErr error
	record := func(run Run, step string, e error) {
		r.logError(ctx, "runner: reaper: "+step, e, "run", run.ID)
		if firstErr == nil {
			firstErr = e
		}
	}

	for _, run := range runs {
		if run.StartedAt.IsZero() {
			continue // not actually started; RequeueOrphanedRuns territory
		}
		job, err := r.store.GetJob(ctx, run.OrgID, run.JobID)
		if err != nil {
			record(run, "load job", err)
			continue
		}
		limit := r.exec.maxRunDuration(job)
		// Written as two comparisons so a saturated limit cannot overflow.
		if elapsed := now.Sub(run.StartedAt); elapsed < reapSlack || elapsed-reapSlack < limit {
			continue // still within its window; the executor will finish it
		}
		termEv, err := r.buildEvent(run, job, "run.failed", StatusFailed, -1)
		if err != nil {
			record(run, "build event", err)
			continue
		}
		reason := fmt.Sprintf("run still 'running' %s past its %s time limit (worker lost)",
			reapSlack, limit)
		reaped, err := r.store.ReapStaleRun(ctx, run.ID, "reaped: "+reason, termEv)
		if err != nil {
			record(run, "fail stale run", err)
			continue
		}
		if reaped {
			r.log().Warn("runner: reaped stale run", "run", run.ID, "job", job.Name, "reason", reason)
			r.fire(ctx, termEv)
		}
	}
	return firstErr
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
// RequeueOrphanedRuns (at-least-once). Runs orphaned while the process stays
// up (worker goroutine lost mid-run) are failed by the periodic reaper
// (ReapOnce) once the run's time limit plus reapSlack has passed, so an orphan
// never blocks its job's no-overlap guard until the next restart.
func (r *Runner) Start(ctx context.Context) error {
	// 1. Reconcile crash-orphaned 'running' runs back to 'pending'.
	requeued, err := r.store.RequeueOrphanedRuns(ctx)
	if err != nil {
		return err
	}
	if requeued > 0 {
		r.log().Info("runner: requeued runs left running by an earlier process", "runs", requeued)
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
				// Tick has logged them.
				_ = r.Tick(ctx)
			}
		}
	}()

	// Stale-run reaper: periodically fail 'running' runs whose worker was lost
	// mid-run, so an orphan can't silently block its job until the next restart.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(reapInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Reap errors are transient (DB hiccups); the next sweep retries.
				// ReapOnce has logged them.
				_ = r.ReapOnce(ctx)
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
					// Transient store/exec error, already logged by
					// claimAndRun: back off one poll interval rather than spin.
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
