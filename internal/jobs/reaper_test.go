package jobs_test

import (
	"context"
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
