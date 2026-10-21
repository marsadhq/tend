package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// unreadableStore is a real store whose DueJobs reports one job as a row it
// could not read, the way the store does for a job whose env or settings do
// not decode (the store's own tests corrupt real rows on both backends).
type unreadableStore struct {
	store.Store
	badID int64
}

func (u unreadableStore) DueJobs(ctx context.Context, now time.Time) ([]jobs.Job, error) {
	due, err := u.Store.DueJobs(ctx, now)
	if err != nil {
		return due, err
	}
	var out []jobs.Job
	var bad []*jobs.UnreadableJobError
	for _, j := range due {
		if j.ID == u.badID {
			bad = append(bad, &jobs.UnreadableJobError{OrgID: j.OrgID, JobID: j.ID, Name: j.Name, Err: errors.New("unmarshal env: invalid character 'n'")})
			continue
		}
		out = append(out, j)
	}
	if len(bad) > 0 {
		return out, &jobs.UnreadableJobsError{Jobs: bad}
	}
	return out, nil
}

// TestTickSkipsUnreadableJob: one job whose row cannot be read must not stop
// the others. The tick schedules the readable ones, logs the unreadable one,
// records a failed run with run.failed for it once, and stops scheduling it.
func TestTickSkipsUnreadableJob(t *testing.T) {
	forEachStore(t, func(t *testing.T, real store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, real)
		due := time.Now().Add(-time.Minute)
		goodID, err := real.CreateJob(ctx, jobs.Job{OrgID: orgID, Name: "good", Type: jobs.Shell, Command: "true", Enabled: true, IntervalSeconds: 60, NextRunAt: due})
		if err != nil {
			t.Fatal(err)
		}
		badID, err := real.CreateJob(ctx, jobs.Job{OrgID: orgID, Name: "broken", Type: jobs.Shell, Command: "true", Enabled: true, IntervalSeconds: 60, NextRunAt: due})
		if err != nil {
			t.Fatal(err)
		}
		s := unreadableStore{Store: real, badID: badID}

		var logs bytes.Buffer
		clk := clock.NewFake(time.Now())
		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clk)
		r.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		var fired []string
		r.EventSink = func(_ context.Context, ev core.Event) { fired = append(fired, ev.Type) }
		if err := r.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if err := r.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		good, err := real.ListRuns(ctx, orgID, goodID, 10)
		if err != nil || len(good) != 1 || good[0].Status != jobs.StatusSucceeded {
			t.Fatalf("the readable job's runs = %+v (%v), want one success", good, err)
		}
		bad, err := real.ListRuns(ctx, orgID, badID, 10)
		if err != nil || len(bad) != 1 || bad[0].Status != jobs.StatusFailed || bad[0].ExitCode != -1 ||
			!strings.Contains(bad[0].Output, "cannot be read") || !strings.Contains(bad[0].Output, "tend job rm broken") {
			t.Fatalf("the unreadable job's runs = %+v (%v), want one failed run saying why and what to do", bad, err)
		}
		if l := logs.String(); !strings.Contains(l, "level=ERROR") || !strings.Contains(l, "job=broken") || !strings.Contains(l, "invalid character") {
			t.Fatalf("not logged as it should be:\n%s", l)
		}
		if len(fired) == 0 || fired[0] != jobs.EventRunFailed {
			t.Fatalf("sink fired %v, want run.failed first", fired)
		}

		// Later ticks run the readable job again and record nothing more for
		// the unreadable one, which is off the schedule.
		clk.Advance(2 * time.Minute)
		if err := r.Tick(ctx); err != nil {
			t.Fatalf("second Tick: %v", err)
		}
		if err := r.DrainOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if bad, _ := real.ListRuns(ctx, orgID, badID, 10); len(bad) != 1 {
			t.Fatalf("the unreadable job has %d runs after a second tick, want 1", len(bad))
		}
		if good, _ := real.ListRuns(ctx, orgID, goodID, 10); len(good) != 2 {
			t.Fatalf("the readable job has %d runs after a second tick, want 2", len(good))
		}
	})
}
