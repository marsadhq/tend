package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// corrupt overwrites column of job id with value, as a bad edit or a bug
// could leave it.
func corrupt(t *testing.T, b backend, id int64, column, value string) {
	t.Helper()
	if _, err := store.RawDB(b.store).ExecContext(context.Background(),
		`UPDATE jobs SET `+column+` = `+ph(b.name, 1)+` WHERE id = `+ph(b.name, 2), value, id); err != nil {
		t.Fatalf("corrupt %s: %v", column, err)
	}
}

// TestUnreadableJobRows: a job whose env or settings cannot be decoded no
// longer makes DueJobs and ListJobs fail for every job. They return the jobs
// they can read and name the others in a *jobs.UnreadableJobsError;
// GetJobByName names it in a *jobs.UnreadableJobError.
func TestUnreadableJobRows(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatal(err)
			}
			due := time.Now().Add(-time.Minute)
			mk := func(j jobs.Job) int64 {
				j.OrgID, j.Enabled, j.NextRunAt = org.ID, true, due
				id, err := s.CreateJob(ctx, j)
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			good := mk(jobs.Job{Name: "good", Type: jobs.Shell, Command: "true"})
			badEnv := mk(jobs.Job{Name: "bad-env", Type: jobs.Shell, Command: "true", Env: map[string]string{"A": "b"}})
			badSpec := mk(jobs.Job{Name: "bad-spec", Type: jobs.TLS, Spec: &jobs.Spec{Host: "example.com", WarnDays: 14}})
			corrupt(t, b, badEnv, "env", "{not json")
			corrupt(t, b, badSpec, "spec", `{"warn_days":"fourteen"}`)

			check := func(what string, got []jobs.Job, err error) {
				t.Helper()
				if len(got) != 1 || got[0].ID != good {
					t.Fatalf("%s returned %+v, want only the readable job", what, got)
				}
				var u *jobs.UnreadableJobsError
				if !errors.As(err, &u) || len(u.Jobs) != 2 {
					t.Fatalf("%s error = %v, want an UnreadableJobsError naming two jobs", what, err)
				}
				for i, want := range []struct {
					id   int64
					name string
				}{{badEnv, "bad-env"}, {badSpec, "bad-spec"}} {
					if u.Jobs[i].JobID != want.id || u.Jobs[i].Name != want.name || u.Jobs[i].OrgID != org.ID {
						t.Errorf("%s unreadable[%d] = %+v, want %s (%d)", what, i, u.Jobs[i], want.name, want.id)
					}
				}
			}
			list, err := s.ListJobs(ctx, org.ID)
			check("ListJobs", list, err)
			dueJobs, err := s.DueJobs(ctx, time.Now())
			check("DueJobs", dueJobs, err)

			_, err = s.GetJobByName(ctx, org.ID, "bad-spec")
			var one *jobs.UnreadableJobError
			if !errors.As(err, &one) || one.JobID != badSpec {
				t.Fatalf("GetJobByName(bad-spec) error = %v, want an UnreadableJobError for job %d", err, badSpec)
			}
			if _, err := s.GetJob(ctx, org.ID, badEnv); !errors.As(err, &one) || one.JobID != badEnv {
				t.Fatalf("GetJob(bad-env) error = %v", err)
			}
		})
	}
}

// TestFailUnreadableJob records a failed run and its run.failed for a job
// that cannot be read, queues its delivery, and takes the job off the
// schedule so the next tick does not record another one.
func TestFailUnreadableJob(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatal(err)
			}
			chID, err := s.CreateChannel(ctx, notify.Channel{OrgID: org.ID, Name: "ops", Kind: kitnotify.Webhook}, "blob")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: chID, EventType: "run.failed", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			id, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "broken", Type: jobs.Shell, Command: "true",
				Enabled: true, NextRunAt: time.Now().Add(-time.Minute), Env: map[string]string{"A": "b"}})
			if err != nil {
				t.Fatal(err)
			}
			corrupt(t, b, id, "env", "{not json")

			ev, err := s.FailUnreadableJob(ctx, org.ID, id, "broken", "FAIL: the job cannot be read\n\nFix it with tend sync.")
			if err != nil {
				t.Fatalf("FailUnreadableJob: %v", err)
			}
			if ev.Type != jobs.EventRunFailed {
				t.Fatalf("event type %q", ev.Type)
			}
			var p struct {
				JobID    int64  `json:"job_id"`
				JobName  string `json:"job_name"`
				Status   string `json:"status"`
				ExitCode int    `json:"exit_code"`
			}
			if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil || p.JobID != id || p.JobName != "broken" || p.Status != "failed" || p.ExitCode != -1 {
				t.Fatalf("payload %s (%v)", ev.Payload, err)
			}
			runs, err := s.ListRuns(ctx, org.ID, id, 10)
			if err != nil || len(runs) != 1 {
				t.Fatalf("ListRuns: %v, %d runs", err, len(runs))
			}
			if r := runs[0]; r.Status != jobs.StatusFailed || r.ExitCode != -1 || !strings.Contains(r.Output, "cannot be read") || r.StartedAt.IsZero() || r.EndedAt.IsZero() {
				t.Fatalf("recorded run = %+v", r)
			}
			ds, err := s.ClaimDueDeliveries(ctx, time.Now().Add(time.Second), time.Minute, 0, 10)
			if err != nil || len(ds) != 1 || ds[0].Event.Type != "run.failed" {
				t.Fatalf("deliveries = %+v (%v), want one run.failed", ds, err)
			}
			// Its alert says why: the detail gives the FAIL line as the
			// verdict, the advice after it as the output.
			var d jobs.FailureDetail
			if err := json.Unmarshal([]byte(ds[0].Event.Detail), &d); err != nil ||
				d.Verdict != "FAIL: the job cannot be read" || d.Output != "Fix it with tend sync." || ev.Detail != ds[0].Event.Detail {
				t.Fatalf("detail %q (%v), returned %q", ds[0].Event.Detail, err, ev.Detail)
			}
			// Off the schedule: no longer due, so no error either.
			if due, err := s.DueJobs(ctx, time.Now()); err != nil || len(due) != 0 {
				t.Fatalf("DueJobs after = %+v, %v", due, err)
			}
		})
	}
}
