package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// TestDeleteJobRecordsJobRemoved pins that deleting a job records job.removed
// with the contract payload and dedup key, in the delete's transaction, and
// that both an org-wide rule and a rule scoped to the deleted job route it.
// A delete of a missing job records nothing.
func TestDeleteJobRecordsJobRemoved(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, jobID := seedJob(t, ctx, s)
		orgWide := seedAlertChannel(t, ctx, s, orgID, "org-wide", "job.removed")
		scoped, err := s.CreateChannel(ctx, notify.Channel{OrgID: orgID, Name: "scoped", Kind: kitnotify.Webhook}, "blob")
		if err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: orgID, ChannelID: scoped, EventType: "job.removed", JobID: jobID, Enabled: true}); err != nil {
			t.Fatalf("CreateRule scoped: %v", err)
		}

		if err := s.DeleteJob(ctx, orgID, jobID+1000); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("DeleteJob(missing) = %v, want ErrNotFound", err)
		}
		if got := eventsOfType(t, ctx, s, orgID, "job.removed"); len(got) != 0 {
			t.Fatalf("a failed delete recorded %d job.removed", len(got))
		}

		if err := s.DeleteJob(ctx, orgID, jobID); err != nil {
			t.Fatalf("DeleteJob: %v", err)
		}
		got := eventsOfType(t, ctx, s, orgID, "job.removed")
		if len(got) != 1 {
			t.Fatalf("job.removed events = %d, want 1", len(got))
		}
		ev := got[0]
		if want := fmt.Sprintf(`{"job_id":%d,"job_name":"claimee"}`, jobID); ev.Payload != want {
			t.Errorf("payload = %s, want %s", ev.Payload, want)
		}
		if want := fmt.Sprintf("job:%d:removed", jobID); ev.DedupKey != want {
			t.Errorf("DedupKey = %q, want %q", ev.DedupKey, want)
		}
		if notify.EventJobID(ev) != jobID {
			t.Errorf("EventJobID = %d, want %d", notify.EventJobID(ev), jobID)
		}
		for _, ch := range []int64{orgWide, scoped} {
			if n := pendingDeliveries(t, ctx, s, backendName(s), ch); n != 1 {
				t.Errorf("pending deliveries for channel %d = %d, want 1", ch, n)
			}
		}
	})
}

// TestDeleteHeartbeatRecordsHeartbeatRemoved pins heartbeat.removed: plain name
// payload, no dedup key, routed by its rules, and nothing for a failed delete.
func TestDeleteHeartbeatRecordsHeartbeatRemoved(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		chID := seedAlertChannel(t, ctx, s, org.ID, "ops", "heartbeat.removed")
		id, _, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: "nightly", Token: "tok-rm", PeriodSeconds: 60})
		if err != nil {
			t.Fatalf("CreateHeartbeat: %v", err)
		}

		if err := s.DeleteHeartbeat(ctx, org.ID+1, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cross-org DeleteHeartbeat = %v, want ErrNotFound", err)
		}
		if got := eventsOfType(t, ctx, s, org.ID, "heartbeat.removed"); len(got) != 0 {
			t.Fatalf("a failed delete recorded %d heartbeat.removed", len(got))
		}

		if err := s.DeleteHeartbeat(ctx, org.ID, id); err != nil {
			t.Fatalf("DeleteHeartbeat: %v", err)
		}
		got := eventsOfType(t, ctx, s, org.ID, "heartbeat.removed")
		if len(got) != 1 {
			t.Fatalf("heartbeat.removed events = %d, want 1", len(got))
		}
		if got[0].Payload != "nightly" || got[0].DedupKey != "" || got[0].Source != "heartbeat" {
			t.Errorf("event = %+v, want payload nightly, no dedup key, source heartbeat", got[0])
		}
		if n := pendingDeliveries(t, ctx, s, backendName(s), chID); n != 1 {
			t.Errorf("pending deliveries = %d, want 1", n)
		}
	})
}

// backendName names s's backend the way ph expects.
func backendName(s store.Store) string {
	if _, ok := s.(*store.PostgresStore); ok {
		return "postgres"
	}
	return "sqlite"
}
