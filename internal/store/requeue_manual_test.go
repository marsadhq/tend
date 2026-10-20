package store_test

import (
	"context"
	"testing"

	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// TestRequeueOrphanedRunsLeavesManualRunsAlone pins that serve's startup
// requeue only takes back runs serve's own runner claimed. A run a `tend run`
// process claimed may still be executing there; requeueing it made serve run
// the same job a second time.
func TestRequeueOrphanedRunsLeavesManualRunsAlone(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, jobID := seedJob(t, ctx, s)
		claim := func(claimant string) int64 {
			t.Helper()
			id, err := s.EnqueueRun(ctx, orgID, jobID)
			if err != nil {
				t.Fatalf("EnqueueRun: %v", err)
			}
			run, ok, err := s.ClaimRun(ctx, claimant)
			if err != nil || !ok || run.ID != id {
				t.Fatalf("ClaimRun(%q) = (%d, %v, %v), want run %d", claimant, run.ID, ok, err, id)
			}
			return id
		}
		servesRun := claim("runner")
		manualRun := claim(jobs.ManualClaimant())

		n, err := s.RequeueOrphanedRuns(ctx)
		if err != nil {
			t.Fatalf("RequeueOrphanedRuns: %v", err)
		}
		if n != 1 {
			t.Fatalf("RequeueOrphanedRuns requeued %d runs, want 1 (serve's own)", n)
		}
		for id, want := range map[int64]jobs.RunStatus{servesRun: jobs.StatusPending, manualRun: jobs.StatusRunning} {
			r, err := s.GetRun(ctx, orgID, id)
			if err != nil {
				t.Fatalf("GetRun %d: %v", id, err)
			}
			if r.Status != want {
				t.Errorf("run %d (claimed by %q): status %s, want %s", id, r.ClaimedBy, r.Status, want)
			}
		}
		manual, err := s.GetRun(ctx, orgID, manualRun)
		if err != nil {
			t.Fatal(err)
		}
		if manual.ClaimedBy != jobs.ManualClaimant() {
			t.Errorf("manual run claimed_by = %q, want %q kept", manual.ClaimedBy, jobs.ManualClaimant())
		}
	})
}
