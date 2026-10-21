package jobs_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/kit/secrets"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
)

// failureDetailOf decodes the detail of the run.failed of the job named name.
func failureDetailOf(t *testing.T, evts []core.Event, name string) (jobs.FailureDetail, bool) {
	t.Helper()
	for _, e := range evts {
		if e.Type != jobs.EventRunFailed || !strings.Contains(e.Payload, `"job_name":"`+name+`"`) {
			continue
		}
		var d jobs.FailureDetail
		if e.Detail == "" {
			return d, false
		}
		if err := json.Unmarshal([]byte(e.Detail), &d); err != nil {
			t.Fatalf("%s: detail %q: %v", name, e.Detail, err)
		}
		return d, true
	}
	t.Fatalf("no run.failed for %s in %+v", name, evts)
	return jobs.FailureDetail{}, false
}

// TestRunFailedCarriesTheOutputTail: the run.failed of a run carries, for its
// alert, the tail of the output the run stored (its secrets already redacted)
// and, when an attempt hit its timeout, that timeout. The stored event and
// the one fired to the sink are the same; a run that succeeds carries none.
func TestRunFailedCarriesTheOutputTail(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	const secret = "s3cr3t-value"
	ct, err := box.Encrypt([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, org.ID, "api", ct); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	for _, j := range []jobs.Job{
		{Name: "chatty", Command: `for i in $(seq 1 15); do echo "line $i"; done; echo "key=$API_KEY" >&2; echo boom >&2; exit 3`,
			Env: map[string]string{"API_KEY": "{{ secret.api }}"}},
		{Name: "slow", Command: "sleep 5", TimeoutSeconds: 1},
		{Name: "fine", Command: "echo ok"},
	} {
		j.OrgID, j.Type, j.Enabled = org.ID, jobs.Shell, true
		id, err := s.CreateJob(ctx, j)
		if err != nil {
			t.Fatalf("CreateJob %s: %v", j.Name, err)
		}
		if _, err := s.EnqueueRun(ctx, org.ID, id); err != nil {
			t.Fatalf("EnqueueRun %s: %v", j.Name, err)
		}
	}
	sink := &fakeSink{}
	r := jobs.NewRunner(s, jobs.NewExecutor(), box, clock.NewFake(time.Now()))
	r.EventSink = sink.fire
	for i := 0; i < 3; i++ {
		if err := r.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
	}
	stored, err := s.ListEvents(ctx, org.ID, 50)
	if err != nil {
		t.Fatal(err)
	}

	for _, evts := range [][]core.Event{stored, sink.snapshot()} {
		chatty, ok := failureDetailOf(t, evts, "chatty")
		if !ok || !strings.HasPrefix(chatty.Output, "line 1\n") || !strings.HasSuffix(chatty.Output, "line 15\nkey=***\nboom") || chatty.TimeoutSeconds != 0 {
			t.Errorf("chatty: detail = %+v (ok=%v), want all 17 lines (an event keeps up to 50), the secret redacted, no timeout", chatty, ok)
		}
		if strings.Contains(chatty.Output, secret) {
			t.Errorf("chatty: the secret reached the detail: %q", chatty.Output)
		}
		if slow, ok := failureDetailOf(t, evts, "slow"); !ok || slow.TimeoutSeconds != 1 {
			t.Errorf("slow: detail = %+v (ok=%v), want its 1s timeout", slow, ok)
		}
	}
	for _, e := range stored {
		if e.Type != jobs.EventRunFailed && e.Detail != "" {
			t.Errorf("%s carries a detail %q; only run.failed does", e.Type, e.Detail)
		}
	}
}

// TestRunFailedTrustsOnlyItsOwnTruncation: a job that prints the text of the
// truncation marker at the end of a short output was not cut. Its output is
// stored as printed, and its alert's tail is its true end, last line and all.
func TestRunFailedTrustsOnlyItsOwnTruncation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, ctx)
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const printed = "uploading\nERROR: disk full\n... [output truncated at 1MiB]"
	id, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "mimic", Type: jobs.Shell, Enabled: true,
		Command: "printf '%s' '" + printed + "'; exit 1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueRun(ctx, org.ID, id); err != nil {
		t.Fatal(err)
	}
	r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.NewFake(time.Now()))
	if err := r.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	runs, err := s.ListRuns(ctx, org.ID, id, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v", err)
	}
	if run, _ := s.GetRun(ctx, org.ID, runs[0].ID); run.Output != printed {
		t.Errorf("stored output %q, want it as printed", run.Output)
	}
	evts, err := s.ListEvents(ctx, org.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := failureDetailOf(t, evts, "mimic"); !ok || d.Truncated || d.Output != printed {
		t.Errorf("detail = %+v (ok=%v), want the whole output and no truncation", d, ok)
	}
}
