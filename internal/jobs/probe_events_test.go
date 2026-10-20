package jobs_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// TestProbesAndDrillsRaiseRunEvents runs an expiry probe and a restore drill
// through the runner and a real store, on both backends: each fails, then
// passes, and must record run.failed and then run.succeeded with
// run.recovered, with the same payload as any job's, the output stored with
// the run, and a delivery queued for a rule routing those events. Nothing on
// the notification side knows these job types exist.
func TestProbesAndDrillsRaiseRunEvents(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		chID, err := s.CreateChannel(ctx, notify.Channel{OrgID: orgID, Name: "ward", Kind: kitnotify.Webhook}, "blob")
		if err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		for _, ev := range []string{"run.failed", "run.recovered"} {
			if _, err := s.CreateRule(ctx, notify.Rule{OrgID: orgID, ChannelID: chID, EventType: ev, Enabled: true}); err != nil {
				t.Fatalf("CreateRule: %v", err)
			}
		}

		flag := filepath.Join(t.TempDir(), "restored-ok")
		cases := []struct {
			job  jobs.Job
			fix  func(j *jobs.Job) // makes the second run pass
			fail string            // in the failed run's output
		}{
			{
				job: jobs.Job{Name: "api-token", Type: jobs.Expiry,
					Spec: &jobs.Spec{ExpiresAt: time.Now().Add(3 * 24 * time.Hour), WarnDays: 14}},
				fix:  func(j *jobs.Job) { j.Spec.ExpiresAt = time.Now().Add(300 * 24 * time.Hour) },
				fail: "FAIL: expires in 2 days",
			},
			{
				job: jobs.Job{Name: "restore-drill", Type: jobs.Drill,
					Spec: &jobs.Spec{Restore: "true", Checks: []string{"test -f " + flag}, ScratchDir: t.TempDir()}},
				fix: func(*jobs.Job) {
					if err := os.WriteFile(flag, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				},
				fail: "FAIL: check 1 failed (exit 1)",
			},
		}

		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.NewFake(time.Now()))
		for _, c := range cases {
			c.job.OrgID, c.job.Enabled = orgID, true
			jobID, err := s.CreateJob(ctx, c.job)
			if err != nil {
				t.Fatalf("CreateJob %s: %v", c.job.Name, err)
			}
			run := func() jobs.Run {
				t.Helper()
				if _, err := s.EnqueueRun(ctx, orgID, jobID); err != nil {
					t.Fatalf("EnqueueRun: %v", err)
				}
				if err := r.DrainOnce(ctx); err != nil {
					t.Fatalf("DrainOnce: %v", err)
				}
				runs, err := s.ListRuns(ctx, orgID, jobID, 1)
				if err != nil || len(runs) != 1 {
					t.Fatalf("ListRuns: %v (%d)", err, len(runs))
				}
				return runs[0]
			}

			failed := run()
			if failed.Status != jobs.StatusFailed || failed.ExitCode != 1 || !strings.Contains(failed.Output, c.fail) {
				t.Fatalf("%s: first run %s exit %d, output:\n%s", c.job.Name, failed.Status, failed.ExitCode, failed.Output)
			}
			j, err := s.GetJob(ctx, orgID, jobID)
			if err != nil {
				t.Fatal(err)
			}
			c.fix(&j)
			if err := s.UpdateJob(ctx, j); err != nil {
				t.Fatal(err)
			}
			passed := run()
			if passed.Status != jobs.StatusSucceeded || !strings.HasPrefix(passed.Output, "ok: ") {
				t.Fatalf("%s: second run %s, output:\n%s", c.job.Name, passed.Status, passed.Output)
			}
		}

		evs, err := s.ListEvents(ctx, orgID, 100)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		seen := map[string]int{}
		for _, e := range evs {
			var p struct {
				JobName  string `json:"job_name"`
				Status   string `json:"status"`
				ExitCode int    `json:"exit_code"`
			}
			if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
				continue
			}
			seen[p.JobName+" "+e.Type+" "+p.Status]++
			if e.Type == "run.failed" && p.ExitCode != 1 {
				t.Errorf("%s run.failed exit_code = %d, want 1", p.JobName, p.ExitCode)
			}
		}
		for _, name := range []string{"api-token", "restore-drill"} {
			for _, want := range []string{"run.failed failed", "run.succeeded succeeded", "run.recovered succeeded"} {
				if seen[name+" "+want] != 1 {
					t.Errorf("%s: %q recorded %d times, want once (events: %v)", name, want, seen[name+" "+want], seen)
				}
			}
		}

		ds, err := s.ClaimDueDeliveries(ctx, time.Now().Add(time.Second), time.Minute, 0, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries: %v", err)
		}
		if len(ds) != 4 {
			t.Fatalf("deliveries = %d, want 4 (a run.failed and a run.recovered per job)", len(ds))
		}
	})
}
