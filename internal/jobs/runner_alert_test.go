package jobs_test

import (
	"context"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/secrets"
)

// fakeSink captures the terminal events the runner fires. It is guarded by a
// mutex because the runner may fire from worker goroutines (Tick+DrainOnce is
// single-goroutine, but the field type is a callback that the autonomous loop
// could invoke concurrently).
type fakeSink struct {
	mu     sync.Mutex
	events []core.Event
}

func (f *fakeSink) fire(_ context.Context, ev core.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakeSink) snapshot() []core.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.Event, len(f.events))
	copy(out, f.events)
	return out
}

// findType returns the first captured event of the given type and whether one
// was found.
func findType(evts []core.Event, typ string) (core.Event, bool) {
	for _, e := range evts {
		if e.Type == typ {
			return e, true
		}
	}
	return core.Event{}, false
}

// TestRunnerFiresSinkOnTerminalFailure: a failing job both STORES a run.failed
// event and fires it to the EventSink with matching org/payload.
func TestRunnerFiresSinkOnTerminalFailure(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fk := clock.NewFake(time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC))

	_, err = s.CreateJob(ctx, jobs.Job{
		OrgID:     org.ID,
		Name:      "failer",
		Type:      jobs.Shell,
		Command:   "exit 1",
		Cron:      "0 3 * * *",
		Enabled:   true,
		NextRunAt: fk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	sink := &fakeSink{}
	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk)
	r.EventSink = sink.fire

	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := r.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}

	// (a) run.failed was durably STORED.
	evts, err := s.ListEvents(ctx, org.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	storedFailed, ok := findType(evts, "run.failed")
	if !ok {
		t.Fatalf("expected a stored run.failed event, got %+v", evts)
	}

	// (b) the sink received a run.failed event matching the stored one.
	fired := sink.snapshot()
	firedFailed, ok := findType(fired, "run.failed")
	if !ok {
		t.Fatalf("sink did not receive a run.failed event, got %+v", fired)
	}
	// The sink must NOT receive non-terminal run.started.
	if _, started := findType(fired, "run.started"); started {
		t.Fatalf("sink should not receive run.started, got %+v", fired)
	}
	if firedFailed.OrgID != org.ID {
		t.Fatalf("fired event OrgID = %d, want %d", firedFailed.OrgID, org.ID)
	}
	if firedFailed.OrgID != storedFailed.OrgID || firedFailed.Payload != storedFailed.Payload {
		t.Fatalf("fired event (org=%d payload=%q) does not match stored (org=%d payload=%q)",
			firedFailed.OrgID, firedFailed.Payload, storedFailed.OrgID, storedFailed.Payload)
	}
}

// TestRunnerFiresSinkOnTerminalSuccess: a succeeding job fires run.succeeded.
func TestRunnerFiresSinkOnTerminalSuccess(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fk := clock.NewFake(time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC))

	if _, err := s.CreateJob(ctx, jobs.Job{
		OrgID:     org.ID,
		Name:      "ok",
		Type:      jobs.Shell,
		Command:   "echo ok",
		Cron:      "0 3 * * *",
		Enabled:   true,
		NextRunAt: fk.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &fakeSink{}
	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk)
	r.EventSink = sink.fire

	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := r.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}

	fired := sink.snapshot()
	if _, ok := findType(fired, "run.succeeded"); !ok {
		t.Fatalf("sink did not receive a run.succeeded event, got %+v", fired)
	}
	// A success must never fire run.failed.
	if _, ok := findType(fired, "run.failed"); ok {
		t.Fatalf("sink should not receive run.failed for a succeeding job, got %+v", fired)
	}
	if _, ok := findType(fired, "run.started"); ok {
		t.Fatalf("sink should not receive run.started, got %+v", fired)
	}

	// The stored terminal event is the same type the sink received.
	evts, err := s.ListEvents(ctx, org.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findType(evts, "run.succeeded"); !ok {
		t.Fatalf("expected a stored run.succeeded event, got %+v", evts)
	}
}

// TestRunnerDoesNotFireWhenSinkNil: with EventSink == nil, a failing job runs
// to completion without panicking (exercises the nil-safe fire helper).
func TestRunnerDoesNotFireWhenSinkNil(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fk := clock.NewFake(time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC))

	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID:     org.ID,
		Name:      "failer-nil",
		Type:      jobs.Shell,
		Command:   "exit 1",
		Cron:      "0 3 * * *",
		Enabled:   true,
		NextRunAt: fk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, fk) // EventSink left nil
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
	if len(runs) != 1 || runs[0].Status != jobs.StatusFailed {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
}

// TestRunnerFiresSinkOnResolveFailure: the secret-resolution-failure path also
// fires run.failed. A job references a secret that does not exist; resolveEnv
// fails before execution, the runner records run.failed via FinishRunAndEmit,
// and the sink receives it.
func TestRunnerFiresSinkOnResolveFailure(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fk := clock.NewFake(time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC))

	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}

	jobID, err := s.CreateJob(ctx, jobs.Job{
		OrgID:     org.ID,
		Name:      "missing-secret",
		Type:      jobs.Shell,
		Command:   "echo $X",
		Cron:      "0 3 * * *",
		Enabled:   true,
		Env:       map[string]string{"X": "{{ secret.missing }}"},
		NextRunAt: fk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	sink := &fakeSink{}
	r := jobs.NewRunner(s, jobs.NewExecutor(), box, fk)
	r.EventSink = sink.fire

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
	if len(runs) != 1 || runs[0].Status != jobs.StatusFailed {
		t.Fatalf("runs = %+v, want one failed run (secret resolution failed)", runs)
	}

	fired := sink.snapshot()
	if _, ok := findType(fired, "run.failed"); !ok {
		t.Fatalf("sink did not receive a run.failed event on resolve failure, got %+v", fired)
	}
}
