package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// ruleWithJob reports whether a rule scoped to (channelID, eventType, jobID)
// appears in the list.
func ruleWithJob(rules []notify.Rule, channelID int64, eventType string, jobID int64) bool {
	for _, r := range rules {
		if r.ChannelID == channelID && r.EventType == eventType && r.JobID == jobID {
			return true
		}
	}
	return false
}

// TestDeleteJobCascadeAndIsolation verifies DeleteJob transactionally removes the
// job, its runs, and its job-scoped notification rules, while leaving an unrelated
// job (with its runs/rules) and any all-jobs rule (job_id=0) untouched.
func TestDeleteJobCascadeAndIsolation(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}

		// jobA: the job under deletion.
		jobA, err := s.CreateJob(ctx, jobs.Job{
			OrgID: org.ID, Name: "job-a", Type: jobs.Shell, Command: "echo a", Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateJob jobA: %v", err)
		}
		if _, err := s.EnqueueRun(ctx, org.ID, jobA); err != nil {
			t.Fatalf("EnqueueRun jobA: %v", err)
		}

		// jobB: unrelated job that must survive.
		jobB, err := s.CreateJob(ctx, jobs.Job{
			OrgID: org.ID, Name: "job-b", Type: jobs.Shell, Command: "echo b", Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateJob jobB: %v", err)
		}
		if _, err := s.EnqueueRun(ctx, org.ID, jobB); err != nil {
			t.Fatalf("EnqueueRun jobB: %v", err)
		}

		// channel_id values are arbitrary ints (notification_rules has no FK to
		// channels), matching the rule CRUD tests.
		const chID int64 = 10

		// Rules: one scoped to jobA (must be deleted), one scoped to jobB (must
		// remain), one all-jobs job_id=0 rule (must remain).
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: chID, EventType: "run.failed", Enabled: true, JobID: jobA}); err != nil {
			t.Fatalf("CreateRule jobA: %v", err)
		}
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: chID, EventType: "run.failed", Enabled: true, JobID: jobB}); err != nil {
			t.Fatalf("CreateRule jobB: %v", err)
		}
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: chID, EventType: "run.failed", Enabled: true, JobID: 0}); err != nil {
			t.Fatalf("CreateRule all-jobs: %v", err)
		}

		// Delete jobA.
		if err := s.DeleteJob(ctx, org.ID, jobA); err != nil {
			t.Fatalf("DeleteJob jobA: %v", err)
		}

		// jobA gone.
		if _, err := s.GetJob(ctx, org.ID, jobA); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetJob(jobA) after delete: err = %v, want ErrNotFound", err)
		}
		// jobA runs gone.
		runsA, err := s.ListRuns(ctx, org.ID, jobA, 0)
		if err != nil {
			t.Fatalf("ListRuns(jobA): %v", err)
		}
		if len(runsA) != 0 {
			t.Fatalf("jobA runs after delete: got %d, want 0", len(runsA))
		}

		// jobB survives with its run.
		if _, err := s.GetJob(ctx, org.ID, jobB); err != nil {
			t.Fatalf("GetJob(jobB) after delete: %v", err)
		}
		runsB, err := s.ListRuns(ctx, org.ID, jobB, 0)
		if err != nil {
			t.Fatalf("ListRuns(jobB): %v", err)
		}
		if len(runsB) != 1 {
			t.Fatalf("jobB runs after delete: got %d, want 1", len(runsB))
		}

		// Rule isolation: jobA-scoped rule gone; jobB-scoped and all-jobs remain.
		rules, err := s.ListRules(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListRules: %v", err)
		}
		if ruleWithJob(rules, chID, "run.failed", jobA) {
			t.Errorf("jobA-scoped rule still present after delete: %+v", rules)
		}
		if !ruleWithJob(rules, chID, "run.failed", jobB) {
			t.Errorf("jobB-scoped rule missing after delete: %+v", rules)
		}
		if !ruleWithJob(rules, chID, "run.failed", 0) {
			t.Errorf("all-jobs rule (job_id=0) missing after delete: %+v", rules)
		}
	})
}

// TestDeleteJobMissingIsErrNotFound verifies deleting a non-existent job returns
// ErrNotFound and leaves existing data untouched (transaction rolled back).
func TestDeleteJobMissingIsErrNotFound(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, jobID := seedJob(t, ctx, s)
		if _, err := s.EnqueueRun(ctx, orgID, jobID); err != nil {
			t.Fatalf("EnqueueRun: %v", err)
		}

		if err := s.DeleteJob(ctx, orgID, 999999); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("DeleteJob(missing): err = %v, want ErrNotFound", err)
		}

		// No side effect: the existing job and its run still present.
		if _, err := s.GetJob(ctx, orgID, jobID); err != nil {
			t.Fatalf("GetJob after missing delete: %v", err)
		}
		runs, err := s.ListRuns(ctx, orgID, jobID, 0)
		if err != nil {
			t.Fatalf("ListRuns after missing delete: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("runs after missing delete: got %d, want 1", len(runs))
		}
	})
}

// TestDeleteJobCrossOrgIsolation verifies a job in org A cannot be deleted via
// org B's id: ErrNotFound is returned and the job remains.
func TestDeleteJobCrossOrgIsolation(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgA, jobID := seedJob(t, ctx, s)
		otherOrg := orgA + 999

		if err := s.DeleteJob(ctx, otherOrg, jobID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("DeleteJob(foreign org): err = %v, want ErrNotFound", err)
		}
		if _, err := s.GetJob(ctx, orgA, jobID); err != nil {
			t.Fatalf("GetJob after cross-org delete: job should remain, got %v", err)
		}
	})
}
