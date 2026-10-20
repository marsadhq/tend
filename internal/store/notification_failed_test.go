package store_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/secrets"
	"github.com/marsadhq/tend/internal/store"
)

// failingProvider fails every send.
type failingProvider struct{}

func (failingProvider) Send(context.Context, notify.Message) error {
	return errors.New("destination down")
}

// testBox returns a Box keyed with an all-zero master key (test-only).
func testBox(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

// TestGivenUpDeliveryIsReportedOnce gives up on a run.failed delivery and
// checks that exactly one notification.failed is recorded and routed to every
// channel with a notification.failed rule, and that giving up on those
// deliveries in turn records nothing more (the loop guard).
func TestGivenUpDeliveryIsReportedOnce(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		box := testBox(t)
		mkChannel := func(name string, events ...string) int64 {
			t.Helper()
			id, err := notify.CreateChannel(ctx, s, box, notify.Channel{OrgID: org.ID, Name: name, Kind: notify.Webhook}, []byte(`{"url":"https://example.invalid/x"}`))
			if err != nil {
				t.Fatalf("CreateChannel %s: %v", name, err)
			}
			for _, ev := range events {
				if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: id, EventType: ev, Enabled: true}); err != nil {
					t.Fatalf("CreateRule: %v", err)
				}
			}
			return id
		}
		mkChannel("broken", "run.failed", "notification.failed")
		mkChannel("backup", "notification.failed")

		if _, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "run.failed", Source: "test", Payload: `{"job_id":1,"job_name":"x"}`}); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}

		w := notify.NewWorker(s, box, func(notify.ChannelType, []byte) (notify.Provider, error) { return failingProvider{}, nil },
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		w.MaxAge = 0 // every failed attempt is final
		if _, err := w.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}

		failed := eventsOfType(t, ctx, s, org.ID, "notification.failed")
		if len(failed) != 1 || failed[0].Payload != "run.failed" {
			t.Fatalf("notification.failed events = %+v, want exactly one for run.failed", failed)
		}
		var pending, finalFailed int
		db := store.RawDB(s)
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE state = 'pending'`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE state = 'failed'`).Scan(&finalFailed); err != nil {
			t.Fatal(err)
		}
		// run.failed -> broken, then notification.failed -> broken and -> backup.
		if pending != 0 || finalFailed != 3 {
			t.Fatalf("deliveries pending=%d failed=%d, want 0 and 3", pending, finalFailed)
		}
	})
}

// TestFailDeliveryAndEmitOnlyOnce pins that a delivery that is no longer
// pending is not given up a second time: no second notification.failed.
func TestFailDeliveryAndEmitOnlyOnce(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		seedAlertChannel(t, ctx, s, org.ID, "ops", "heartbeat.missed")
		if _, err := s.EmitEvent(ctx, core.Event{OrgID: org.ID, Type: "heartbeat.missed", Source: "heartbeat", Payload: "nightly"}); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}
		var id int64
		if err := store.RawDB(s).QueryRowContext(ctx, `SELECT id FROM deliveries`).Scan(&id); err != nil {
			t.Fatalf("read delivery id: %v", err)
		}
		ev := core.Event{OrgID: org.ID, Type: "notification.failed", Source: "notify", Payload: "heartbeat.missed"}
		for i := 0; i < 2; i++ {
			if err := s.FailDeliveryAndEmit(ctx, id, ev); err != nil {
				t.Fatalf("FailDeliveryAndEmit #%d: %v", i+1, err)
			}
		}
		if got := eventsOfType(t, ctx, s, org.ID, "notification.failed"); len(got) != 1 {
			t.Fatalf("notification.failed events = %d, want 1", len(got))
		}
	})
}
