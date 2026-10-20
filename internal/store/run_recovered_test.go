package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// finishRun enqueues, claims and finishes one run of jobID with status,
// recording the matching terminal event the way the runner does.
func finishRun(t *testing.T, ctx context.Context, s store.Store, orgID, jobID int64, status jobs.RunStatus) int64 {
	t.Helper()
	runID, err := s.EnqueueRun(ctx, orgID, jobID)
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	run, ok, err := s.ClaimRun(ctx, "runner")
	if err != nil || !ok || run.ID != runID {
		t.Fatalf("ClaimRun = (%d, %v, %v), want run %d", run.ID, ok, err, runID)
	}
	typ := jobs.EventRunFailed
	if status == jobs.StatusSucceeded {
		typ = jobs.EventRunSucceeded
	}
	ev := core.Event{
		OrgID: orgID, Type: typ, Source: "jobs.runner",
		Payload:  fmt.Sprintf(`{"run_id":%d,"job_id":%d,"job_name":"j","status":%q,"exit_code":0}`, runID, jobID, status),
		DedupKey: fmt.Sprintf("run:%d:%s", runID, typ),
	}
	if _, err := s.FinishRunAndEmit(ctx, runID, status, 0, 1, "", ev); err != nil {
		t.Fatalf("FinishRunAndEmit(%s): %v", status, err)
	}
	return runID
}

// recoveredFor returns the run.recovered events whose dedup key names runID.
func recoveredFor(t *testing.T, ctx context.Context, s store.Store, orgID, runID int64) []core.Event {
	t.Helper()
	var out []core.Event
	for _, e := range eventsOfType(t, ctx, s, orgID, "run.recovered") {
		if e.DedupKey == fmt.Sprintf("run:%d:run.recovered", runID) {
			out = append(out, e)
		}
	}
	return out
}

// TestFinishRunAndEmitRecordsRecovery pins when a success counts as a
// recovery: only when the job's previous finished run did not succeed,
// whichever way it ended (failed, timed out, reaped).
func TestFinishRunAndEmitRecordsRecovery(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, jobID := seedJob(t, ctx, s)

		first := finishRun(t, ctx, s, orgID, jobID, jobs.StatusSucceeded)
		if got := recoveredFor(t, ctx, s, orgID, first); len(got) != 0 {
			t.Fatalf("a job's first success recorded %d run.recovered, want 0", len(got))
		}

		finishRun(t, ctx, s, orgID, jobID, jobs.StatusFailed)
		afterFailed := finishRun(t, ctx, s, orgID, jobID, jobs.StatusSucceeded)
		if got := recoveredFor(t, ctx, s, orgID, afterFailed); len(got) != 1 {
			t.Fatalf("success after a failure recorded %d run.recovered, want 1", len(got))
		}

		again := finishRun(t, ctx, s, orgID, jobID, jobs.StatusSucceeded)
		if got := recoveredFor(t, ctx, s, orgID, again); len(got) != 0 {
			t.Fatalf("success after a success recorded %d run.recovered, want 0", len(got))
		}

		finishRun(t, ctx, s, orgID, jobID, jobs.StatusTimedOut)
		afterTimeout := finishRun(t, ctx, s, orgID, jobID, jobs.StatusSucceeded)
		if got := recoveredFor(t, ctx, s, orgID, afterTimeout); len(got) != 1 {
			t.Fatalf("success after a timeout recorded %d run.recovered, want 1", len(got))
		}

		// A reaped run: claimed, then failed by the reaper.
		reapID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatalf("EnqueueRun: %v", err)
		}
		if _, ok, err := s.ClaimRun(ctx, "runner"); err != nil || !ok {
			t.Fatalf("ClaimRun: ok=%v err=%v", ok, err)
		}
		if reaped, err := s.ReapStaleRun(ctx, reapID, "reaped", core.Event{OrgID: orgID, Type: jobs.EventRunFailed}); err != nil || !reaped {
			t.Fatalf("ReapStaleRun: reaped=%v err=%v", reaped, err)
		}
		afterReap := finishRun(t, ctx, s, orgID, jobID, jobs.StatusSucceeded)
		if got := recoveredFor(t, ctx, s, orgID, afterReap); len(got) != 1 {
			t.Fatalf("success after a reaped run recorded %d run.recovered, want 1", len(got))
		}

		// Another job's failure does not make this job's success a recovery.
		otherID, err := s.CreateJob(ctx, jobs.Job{OrgID: orgID, Name: "other", Type: jobs.Shell, Command: "true", Enabled: true})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		finishRun(t, ctx, s, orgID, otherID, jobs.StatusFailed)
		mine := finishRun(t, ctx, s, orgID, jobID, jobs.StatusSucceeded)
		if got := recoveredFor(t, ctx, s, orgID, mine); len(got) != 0 {
			t.Fatalf("another job's failure made this success a recovery (%d events)", len(got))
		}
	})
}
