package jobs_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/secrets"
	"github.com/marsadhq/tend/internal/store"
)

// newStore opens a fresh, migrated SQLite store in a temp dir. SQLite is
// sufficient for runner tests; the store layer's cross-backend correctness is
// proven separately in the store package.
func newStore(t *testing.T, ctx context.Context) *store.SQLiteStore {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// hasType reports whether any event in evts has the given type.
func hasType(evts []core.Event, typ string) bool {
	for _, e := range evts {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// TestRunnerExecutesDueJobAndEmitsEvents is the proof of life: a due job is
// enqueued, executed, recorded, lifecycle events are emitted, and the schedule
// advances.
func TestRunnerExecutesDueJobAndEmitsEvents(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fk := clock.NewFake(time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC))

	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID:     org.ID,
		Name:      "nightly",
		Type:      jobs.Shell,
		Command:   "echo ok",
		Cron:      "0 3 * * *",
		Enabled:   true,
		NextRunAt: fk.Now(), // due now (job-creation path sets the initial NextRunAt)
	})
	if err != nil {
		t.Fatal(err)
	}

	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk)
	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := r.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}

	runs, err := s.ListRuns(ctx, org.ID, jobID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns len = %d, want 1: %+v", len(runs), runs)
	}
	if runs[0].Status != jobs.StatusSucceeded {
		t.Fatalf("run status = %q, want succeeded", runs[0].Status)
	}
	if !strings.Contains(runs[0].Output, "ok") {
		t.Fatalf("run output = %q, want it to contain %q", runs[0].Output, "ok")
	}

	evts, err := s.ListEvents(ctx, org.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !hasType(evts, "run.started") {
		t.Fatalf("expected a run.started event, got %+v", evts)
	}
	if !hasType(evts, "run.succeeded") {
		t.Fatalf("expected a run.succeeded event, got %+v", evts)
	}

	// Schedule advanced to the next cron occurrence.
	got, err := s.GetJob(ctx, org.ID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	wantNext := time.Date(2026, 5, 30, 3, 0, 0, 0, time.UTC)
	if !got.NextRunAt.Equal(wantNext) {
		t.Fatalf("NextRunAt = %v, want %v", got.NextRunAt, wantNext)
	}
}

// TestRunnerSkipsJobWithInflightRun proves Tick does not double-enqueue a job
// that already has a pending run (DueJobs's no-overlap guard).
func TestRunnerSkipsJobWithInflightRun(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, _ := s.BootstrapDefaultOrg(ctx)
	fk := clock.NewFake(time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC))

	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID:     org.ID,
		Name:      "inflight",
		Type:      jobs.Shell,
		Command:   "echo hi",
		Cron:      "0 3 * * *",
		Enabled:   true,
		NextRunAt: fk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A pending run already exists.
	if _, err := s.EnqueueRun(ctx, org.ID, jobID); err != nil {
		t.Fatal(err)
	}

	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk)
	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	runs, err := s.ListRuns(ctx, org.ID, jobID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns len = %d, want 1 (no double-enqueue): %+v", len(runs), runs)
	}
}

// TestRunnerInjectsAndRedactsSecret proves the secret was injected into the
// job's env (the command echoed it) AND that the stored output is redacted.
func TestRunnerInjectsAndRedactsSecret(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, _ := s.BootstrapDefaultOrg(ctx)
	fk := clock.NewFake(time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC))

	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	ct, err := box.Encrypt([]byte("s3cr3t-value"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := s.PutSecret(ctx, org.ID, "api_key", ct); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID:     org.ID,
		Name:      "withsecret",
		Type:      jobs.Shell,
		Command:   "echo key=$API_KEY",
		Cron:      "0 3 * * *",
		Enabled:   true,
		Env:       map[string]string{"API_KEY": "{{ secret.api_key }}"},
		NextRunAt: fk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	r := jobs.NewRunner(s, jobs.NewExecutor(), box, fk)
	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := r.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}

	runs, err := s.ListRuns(ctx, org.ID, jobID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns len = %d, want 1", len(runs))
	}
	if runs[0].Status != jobs.StatusSucceeded {
		t.Fatalf("run status = %q, want succeeded; output=%q", runs[0].Status, runs[0].Output)
	}
	if strings.Contains(runs[0].Output, "s3cr3t-value") {
		t.Fatalf("output leaked the secret value: %q", runs[0].Output)
	}
	if !strings.Contains(runs[0].Output, "***") {
		// If injection had failed, $API_KEY would be empty and there would be
		// nothing to redact.
		t.Fatalf("output should contain *** (proving inject+redact): %q", runs[0].Output)
	}

	// Secret material must not appear in any event payload.
	evts, err := s.ListEvents(ctx, org.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evts {
		if strings.Contains(e.Payload, "s3cr3t-value") {
			t.Fatalf("event payload leaked secret: %+v", e)
		}
	}
}

// TestRequeueOrphanedRunsViaStore proves the store's RequeueOrphanedRuns method
// re-queues a crash-orphaned 'running' run back to 'pending'. (Start's own
// reconcile call is covered by the end-to-end smoke test TestRunnerStartRunsEndToEnd.)
func TestRequeueOrphanedRunsViaStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, _ := s.BootstrapDefaultOrg(ctx)
	jobID, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "orphan", Type: jobs.Shell, Command: "echo hi", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := s.EnqueueRun(ctx, org.ID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	// Claim it -> 'running' (then "crash").
	if _, ok, err := s.ClaimRun(ctx, "runner"); err != nil || !ok {
		t.Fatalf("ClaimRun: ok=%v err=%v", ok, err)
	}

	if _, err := s.RequeueOrphanedRuns(ctx); err != nil {
		t.Fatalf("RequeueOrphanedRuns: %v", err)
	}

	runs, err := s.ListRuns(ctx, org.ID, jobID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != runID || runs[0].Status != jobs.StatusPending {
		t.Fatalf("after reconcile runs = %+v, want one pending run %d", runs, runID)
	}
}

// TestRunnerStartRunsEndToEnd drives the autonomous loop on a real clock: Start
// reconciles, ticks, and workers drain a due job, then shuts down promptly on
// context cancellation.
func TestRunnerStartRunsEndToEnd(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, _ := s.BootstrapDefaultOrg(ctx)
	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID:     org.ID,
		Name:      "loop",
		Type:      jobs.Shell,
		Command:   "echo started",
		Cron:      "0 3 * * *",
		Enabled:   true,
		NextRunAt: time.Now(), // due now under the real clock
	})
	if err != nil {
		t.Fatal(err)
	}

	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.RealClock{})
	r.TickInterval = 50 * time.Millisecond
	r.Workers = 1

	runCtx, cancel := context.WithCancel(ctx)
	startErr := make(chan error, 1)
	go func() { startErr <- r.Start(runCtx) }()

	// Poll until a succeeded run appears.
	deadline := time.Now().Add(5 * time.Second)
	var succeeded bool
	for time.Now().Before(deadline) {
		runs, err := s.ListRuns(ctx, org.ID, jobID, 10)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(runs) > 0 && runs[0].Status == jobs.StatusSucceeded {
			succeeded = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !succeeded {
		t.Fatal("no succeeded run appeared within timeout")
	}

	// Shut down and confirm Start returns promptly.
	cancel()
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return within 2s of cancellation")
	}
}

// payloadStatus extracts the "status" field from an event's JSON payload.
// Returns "" if the payload is not parseable or the field is absent.
func payloadStatus(e core.Event) string {
	var p struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
		return ""
	}
	return p.Status
}

// TestTimedOutJobEmitsRunFailedWithTimedOutStatus proves I1: a job that exceeds
// its timeout emits a "run.failed" event (not "run.timed_out") whose payload
// status field is "timed_out". Uses a ~1s timeout so the test stays fast.
func TestTimedOutJobEmitsRunFailedWithTimedOutStatus(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fk := clock.NewFake(time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC))

	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID:          org.ID,
		Name:           "sleeper",
		Type:           jobs.Shell,
		Command:        "sleep 5",
		Cron:           "0 3 * * *",
		Enabled:        true,
		TimeoutSeconds: 1, // kill after 1 second
		NextRunAt:      fk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	ex := jobs.NewExecutor()
	ex.Backoff = func(int) time.Duration { return 0 } // instant retries (no retries here)
	r := jobs.NewRunner(s, ex, nil, fk)

	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := r.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}

	runs, err := s.ListRuns(ctx, org.ID, jobID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns len = %d, want 1", len(runs))
	}
	if runs[0].Status != jobs.StatusTimedOut {
		t.Fatalf("run status = %q, want timed_out", runs[0].Status)
	}

	evts, err := s.ListEvents(ctx, org.ID, 10)
	if err != nil {
		t.Fatal(err)
	}

	// I1: type must be run.failed (not run.timed_out)
	if hasType(evts, "run.timed_out") {
		t.Fatal("got forbidden event type run.timed_out; must be run.failed")
	}
	if !hasType(evts, "run.failed") {
		t.Fatalf("expected a run.failed event, got %+v", evts)
	}

	// I1: payload status must be "timed_out" so consumers can distinguish the cause
	var failedEvt core.Event
	for _, e := range evts {
		if e.Type == "run.failed" {
			failedEvt = e
			break
		}
	}
	if ps := payloadStatus(failedEvt); ps != "timed_out" {
		t.Fatalf("run.failed payload status = %q, want %q", ps, "timed_out")
	}
}
