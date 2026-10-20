package cli_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/core"
)

// eventsOfType opens the CLI's database and returns its default org's events
// of type typ.
func eventsOfType(t *testing.T, dsn, typ string) []core.Event {
	t.Helper()
	ctx := context.Background()
	st := openTestStore(t, dsn)
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	evs, err := st.ListEvents(ctx, org.ID, 200)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var out []core.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// TestJobRmRecordsJobRemoved: `tend job rm` records job.removed for the job.
func TestJobRmRecordsJobRemoved(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"job", "add", "-name", "doomed", "-command", "true"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}
	if err := cli.Run(ctx, cfg, []string{"job", "rm", "doomed"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("job rm: %v\nstderr: %s", err, stderr.String())
	}
	got := eventsOfType(t, cfg.DSN, "job.removed")
	if len(got) != 1 || got[0].Payload != `{"job_id":1,"job_name":"doomed"}` || got[0].DedupKey != "job:1:removed" {
		t.Fatalf("job.removed events = %+v, want one for job 1 (doomed)", got)
	}
}

// TestHeartbeatRmRecordsHeartbeatRemoved: `tend heartbeat rm` records
// heartbeat.removed carrying the heartbeat name.
func TestHeartbeatRmRecordsHeartbeatRemoved(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "add", "-name", "doomed-hb", "-period", "60"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat add: %v\nstderr: %s", err, stderr.String())
	}
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "rm", "doomed-hb"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat rm: %v\nstderr: %s", err, stderr.String())
	}
	got := eventsOfType(t, cfg.DSN, "heartbeat.removed")
	if len(got) != 1 || got[0].Payload != "doomed-hb" || got[0].DedupKey != "" {
		t.Fatalf("heartbeat.removed events = %+v, want one for doomed-hb", got)
	}
}
