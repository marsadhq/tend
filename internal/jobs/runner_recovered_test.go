package jobs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// TestRunnerRecordsRunRecovered runs one job that fails, then succeeds, then
// succeeds again. The first success after the failure must be recorded as
// run.recovered as well as run.succeeded, with the same payload and its own
// dedup key, and must enqueue a delivery for a run.recovered rule; the second
// success is only run.succeeded.
func TestRunnerRecordsRunRecovered(t *testing.T) {
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		flag := filepath.Join(t.TempDir(), "ok")

		jobID, err := s.CreateJob(ctx, jobs.Job{
			OrgID: orgID, Name: "flaky", Type: jobs.Shell, Command: "test -f " + flag, Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		chID, err := s.CreateChannel(ctx, notify.Channel{OrgID: orgID, Name: "ops", Kind: notify.Webhook}, "blob")
		if err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: orgID, ChannelID: chID, EventType: "run.recovered", Enabled: true}); err != nil {
			t.Fatalf("CreateRule: %v", err)
		}

		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.NewFake(time.Now()))
		runOnce := func() int64 {
			t.Helper()
			id, err := s.EnqueueRun(ctx, orgID, jobID)
			if err != nil {
				t.Fatalf("EnqueueRun: %v", err)
			}
			if err := r.DrainOnce(ctx); err != nil {
				t.Fatalf("DrainOnce: %v", err)
			}
			return id
		}

		runOnce() // fails: the flag file does not exist yet
		if err := os.WriteFile(flag, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		recoveredRun := runOnce()
		runOnce() // succeeds again: not a recovery

		evs, err := s.ListEvents(ctx, orgID, 100)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		var recovered, succeeded []core.Event
		for _, e := range evs {
			switch e.Type {
			case "run.recovered":
				recovered = append(recovered, e)
			case "run.succeeded":
				succeeded = append(succeeded, e)
			}
		}
		if len(succeeded) != 2 {
			t.Fatalf("run.succeeded events = %d, want 2", len(succeeded))
		}
		if len(recovered) != 1 {
			t.Fatalf("run.recovered events = %d, want exactly 1", len(recovered))
		}
		ev := recovered[0]
		if want := fmt.Sprintf("run:%d:run.recovered", recoveredRun); ev.DedupKey != want {
			t.Errorf("DedupKey = %q, want %q", ev.DedupKey, want)
		}
		if ev.Source != "jobs.runner" {
			t.Errorf("Source = %q, want jobs.runner", ev.Source)
		}
		var p struct {
			RunID    int64  `json:"run_id"`
			JobID    int64  `json:"job_id"`
			JobName  string `json:"job_name"`
			Status   string `json:"status"`
			ExitCode int    `json:"exit_code"`
		}
		if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
			t.Fatalf("payload %q is not JSON: %v", ev.Payload, err)
		}
		if p.RunID != recoveredRun || p.JobID != jobID || p.JobName != "flaky" || p.Status != "succeeded" || p.ExitCode != 0 {
			t.Errorf("payload = %+v, want run %d of job %d (flaky), succeeded, exit 0", p, recoveredRun, jobID)
		}

		ds, err := s.ClaimDueDeliveries(ctx, time.Now().Add(time.Second), time.Minute, notify.DefaultHold, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries: %v", err)
		}
		if len(ds) != 1 || ds[0].ChannelID != chID || ds[0].Event.Type != "run.recovered" {
			t.Fatalf("deliveries = %+v, want one run.recovered delivery to channel %d", ds, chID)
		}
	})
}
