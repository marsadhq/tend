package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// TestEmitEventEnqueuesDeliveries proves the durability core: an alertable
// event with matching enabled rules enqueues one pending delivery per matched
// channel in the same transaction, deduplicating channels across overlapping
// rules; non-alertable and unmatched events enqueue nothing.
func TestEmitEventEnqueuesDeliveries(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}

		// Channel ch10: org-wide rule AND a job-scoped rule (dedupe case).
		// Channel ch11: rule for another event type (must not match).
		// Channel ch12: disabled rule (must not match).
		ch10, ch11, ch12 := newChannel(t, ctx, s, org.ID, "ch10"), newChannel(t, ctx, s, org.ID, "ch11"), newChannel(t, ctx, s, org.ID, "ch12")
		mustRule := func(chID int64, evType string, jobID int64, enabled bool) {
			t.Helper()
			if _, err := s.CreateRule(ctx, notify.Rule{
				OrgID: org.ID, ChannelID: chID, EventType: evType, JobID: jobID, Enabled: enabled,
			}); err != nil {
				t.Fatalf("CreateRule: %v", err)
			}
		}
		mustRule(ch10, "run.failed", 0, true)
		mustRule(ch10, "run.failed", 42, true)
		mustRule(ch11, "heartbeat.missed", 0, true)
		mustRule(ch12, "run.failed", 0, false)

		if _, err := s.EmitEvent(ctx, core.Event{
			OrgID: org.ID, Type: "run.failed", Source: "test",
			Payload: `{"job_id":42,"job_name":"x"}`,
		}); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}

		due, err := s.ClaimDueDeliveries(ctx, time.Now(), time.Minute, notify.DefaultHold, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries: %v", err)
		}
		if len(due) != 1 {
			t.Fatalf("claimed %d deliveries, want exactly 1 (deduped channel ch10)", len(due))
		}
		d := due[0]
		if d.ChannelID != ch10 {
			t.Errorf("channel: got %d want %d", d.ChannelID, ch10)
		}
		if d.Attempts != 1 {
			t.Errorf("attempts after claim: got %d want 1", d.Attempts)
		}
		if d.Event.Type != "run.failed" || d.Event.OrgID != org.ID {
			t.Errorf("joined event: %+v", d.Event)
		}

		// The claim leased the row into the future: nothing is due now.
		again, err := s.ClaimDueDeliveries(ctx, time.Now(), time.Minute, notify.DefaultHold, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries again: %v", err)
		}
		if len(again) != 0 {
			t.Fatalf("re-claimed %d leased deliveries, want 0", len(again))
		}

		// ...but it becomes due again once the lease expires (crash recovery).
		after, err := s.ClaimDueDeliveries(ctx, time.Now().Add(2*time.Minute), time.Minute, notify.DefaultHold, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries after lease: %v", err)
		}
		if len(after) != 1 || after[0].Attempts != 2 {
			t.Fatalf("post-lease claim: got %d rows (attempts=%v), want 1 with attempts=2",
				len(after), after)
		}

		// A non-alertable event never enqueues - even with a matching rule
		// shape for it.
		mustRule(ch10, "run.succeeded", 0, true)
		if _, err := s.EmitEvent(ctx, core.Event{
			OrgID: org.ID, Type: "run.succeeded", Source: "jobs.runner", Payload: `{"job_id":42}`,
		}); err != nil {
			t.Fatalf("EmitEvent run.succeeded: %v", err)
		}
		none, err := s.ClaimDueDeliveries(ctx, time.Now().Add(10*time.Minute), time.Minute, notify.DefaultHold, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries: %v", err)
		}
		for _, d := range none {
			if d.Event.Type == "run.succeeded" {
				t.Fatal("run.succeeded enqueued a delivery")
			}
		}
	})
}

// TestDeliveryFinalization exercises the worker-facing state transitions:
// delivered and failed rows leave the due set; rescheduling moves the due time.
func TestDeliveryFinalization(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		if _, err := s.CreateRule(ctx, notify.Rule{
			OrgID: org.ID, ChannelID: newChannel(t, ctx, s, org.ID, "ops"), EventType: "heartbeat.missed", Enabled: true,
		}); err != nil {
			t.Fatalf("CreateRule: %v", err)
		}
		emit := func() {
			t.Helper()
			if _, err := s.EmitEvent(ctx, core.Event{
				OrgID: org.ID, Type: "heartbeat.missed", Source: "test", Payload: "hb",
			}); err != nil {
				t.Fatalf("EmitEvent: %v", err)
			}
		}
		claimOne := func(at time.Time) notify.Delivery {
			t.Helper()
			ds, err := s.ClaimDueDeliveries(ctx, at, time.Minute, notify.DefaultHold, 10)
			if err != nil || len(ds) != 1 {
				t.Fatalf("ClaimDueDeliveries: %v (%d rows)", err, len(ds))
			}
			return ds[0]
		}

		now := time.Now()

		// delivered -> never due again. (Claim times sit strictly after the
		// enqueue's wall-clock next_attempt_at, hence the +1s.)
		emit()
		d := claimOne(now.Add(time.Second))
		if err := s.MarkDeliveryDelivered(ctx, d.ID); err != nil {
			t.Fatalf("MarkDeliveryDelivered: %v", err)
		}

		// failed -> never due again.
		emit()
		d = claimOne(now.Add(2 * time.Minute))
		if err := s.FailDelivery(ctx, d.ID); err != nil {
			t.Fatalf("FailDelivery: %v", err)
		}

		// rescheduled -> due exactly from the new time.
		emit()
		d = claimOne(now.Add(4 * time.Minute))
		resumeAt := now.Add(30 * time.Minute)
		if err := s.RescheduleDelivery(ctx, d.ID, resumeAt); err != nil {
			t.Fatalf("RescheduleDelivery: %v", err)
		}

		if ds, err := s.ClaimDueDeliveries(ctx, resumeAt.Add(-time.Second), time.Minute, notify.DefaultHold, 10); err != nil || len(ds) != 0 {
			t.Fatalf("before resumeAt: %v (%d rows), want 0", err, len(ds))
		}
		ds, err := s.ClaimDueDeliveries(ctx, resumeAt.Add(time.Second), time.Minute, notify.DefaultHold, 10)
		if err != nil || len(ds) != 1 || ds[0].ID != d.ID {
			t.Fatalf("after resumeAt: %v (%d rows), want the rescheduled delivery", err, len(ds))
		}
	})
}

// newChannel creates a real channel (the enqueue only routes to channels that
// exist) and returns its ID.
func newChannel(t *testing.T, ctx context.Context, s store.Store, orgID int64, name string) int64 {
	t.Helper()
	id, err := s.CreateChannel(ctx, notify.Channel{OrgID: orgID, Name: name, Kind: notify.Webhook}, "blob")
	if err != nil {
		t.Fatalf("CreateChannel %q: %v", name, err)
	}
	return id
}
