package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/store"
)

// TestEventDetailRoundTrips: an event's Detail is stored with it and comes
// back from every read path, the delivery claim included, while an event
// without one reads back empty, so a plain event looks exactly as before.
func TestEventDetailRoundTrips(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		seedAlertChannel(t, ctx, s, org.ID, "hook", "heartbeat.missed")

		const detail = `{"reason":"fail","exit_status":3}`
		withDetail := core.Event{OrgID: org.ID, Type: "heartbeat.missed", Source: "heartbeat", Payload: "backup", Detail: detail}
		if _, err := s.EmitEvent(ctx, withDetail); err != nil {
			t.Fatalf("EmitEvent with detail: %v", err)
		}
		plain := core.Event{OrgID: org.ID, Type: "heartbeat.missed", Source: "heartbeat", Payload: "other"}
		if _, err := s.EmitEvent(ctx, plain); err != nil {
			t.Fatalf("EmitEvent plain: %v", err)
		}

		want := map[string]string{"backup": detail, "other": ""}
		check := func(where string, evs []core.Event) {
			t.Helper()
			if len(evs) != 2 {
				t.Fatalf("%s: got %d events, want 2", where, len(evs))
			}
			for _, e := range evs {
				if e.Detail != want[e.Payload] {
					t.Errorf("%s: event %q detail = %q, want %q", where, e.Payload, e.Detail, want[e.Payload])
				}
			}
		}

		evs, err := s.ListEvents(ctx, org.ID, 10)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		check("ListEvents", evs)

		var hist []core.Event
		for _, name := range []string{"backup", "other"} {
			h, err := s.ListHeartbeatEvents(ctx, org.ID, name, 10)
			if err != nil {
				t.Fatalf("ListHeartbeatEvents: %v", err)
			}
			hist = append(hist, h...)
		}
		check("ListHeartbeatEvents", hist)

		ds, err := s.ClaimDueDeliveries(ctx, time.Now().Add(time.Minute), time.Minute, 0, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries: %v", err)
		}
		var claimed []core.Event
		for _, d := range ds {
			claimed = append(claimed, d.Event)
		}
		check("ClaimDueDeliveries", claimed)
	})
}
