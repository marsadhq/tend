package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/kit/event"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// TestClaimHoldsBackAChannelsLaterDeliveries pins head-of-line ordering per
// channel: while a channel's oldest delivery is pending (claimed, or backing
// off after a failure), the deliveries behind it on that channel cannot be
// claimed; other channels are unaffected. Before, every due row was claimed
// by id, so a delivery queued behind a failing one could overtake it.
func TestClaimHoldsBackAChannelsLaterDeliveries(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		x := seedAlertChannel(t, ctx, s, org.ID, "x", "heartbeat.missed", "heartbeat.recovered")
		y := seedAlertChannel(t, ctx, s, org.ID, "y", "run.failed")

		emit := func(ev core.Event) {
			t.Helper()
			if _, err := s.EmitEvent(ctx, ev); err != nil {
				t.Fatalf("EmitEvent %s: %v", ev.Type, err)
			}
		}
		emit(heartbeat.MissedEvent(org.ID, "nightly"))                                               // x, first
		emit(core.Event{OrgID: org.ID, Type: "run.failed", Source: "test", Payload: `{"job_id":1}`}) // y
		emit(heartbeat.RecoveredEvent(org.ID, "nightly"))                                            // x, second

		now := time.Now().Add(time.Second)
		first, err := s.ClaimDueDeliveries(ctx, now, time.Minute, kitnotify.DefaultHold, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries: %v", err)
		}
		if len(first) != 2 || first[0].ChannelID != x || first[0].Event.Type != "heartbeat.missed" || first[1].ChannelID != y {
			t.Fatalf("first claim = %v, want x's heartbeat.missed and y's run.failed only", claimed(first))
		}

		// The missed delivery failed and backs off for a second.
		if err := s.RescheduleDelivery(ctx, first[0].ID, now.Add(time.Second)); err != nil {
			t.Fatalf("RescheduleDelivery: %v", err)
		}
		if err := s.MarkDeliveryDelivered(ctx, first[1].ID); err != nil {
			t.Fatalf("MarkDeliveryDelivered: %v", err)
		}
		if got, err := s.ClaimDueDeliveries(ctx, now, time.Minute, kitnotify.DefaultHold, 10); err != nil || len(got) != 0 {
			t.Fatalf("claim while x's head backs off = %v (err %v), want nothing: recovered must wait for missed", claimed(got), err)
		}

		// Its retry comes due: the missed delivery goes again, still alone.
		retry, err := s.ClaimDueDeliveries(ctx, now.Add(2*time.Second), time.Minute, kitnotify.DefaultHold, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries: %v", err)
		}
		if len(retry) != 1 || retry[0].Event.Type != "heartbeat.missed" {
			t.Fatalf("retry claim = %v, want only x's heartbeat.missed", claimed(retry))
		}
		if err := s.MarkDeliveryDelivered(ctx, retry[0].ID); err != nil {
			t.Fatalf("MarkDeliveryDelivered: %v", err)
		}

		// Only now may the recovered delivery go.
		next, err := s.ClaimDueDeliveries(ctx, now.Add(2*time.Second), time.Minute, kitnotify.DefaultHold, 10)
		if err != nil {
			t.Fatalf("ClaimDueDeliveries: %v", err)
		}
		if len(next) != 1 || next[0].Event.Type != "heartbeat.recovered" {
			t.Fatalf("claim after missed was delivered = %v, want x's heartbeat.recovered", claimed(next))
		}

		// A given-up head releases its channel too.
		emit(heartbeat.MissedEvent(org.ID, "weekly"))
		if err := s.FailDelivery(ctx, next[0].ID); err != nil {
			t.Fatalf("FailDelivery: %v", err)
		}
		after, err := s.ClaimDueDeliveries(ctx, now.Add(2*time.Second), time.Minute, kitnotify.DefaultHold, 10)
		if err != nil || len(after) != 1 || after[0].Event.Payload != "weekly" {
			t.Fatalf("claim after a give-up = %v (err %v), want the weekly miss", claimed(after), err)
		}
	})
}

// claimed summarises deliveries for a failure message.
func claimed(ds []notify.Delivery) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, fmt.Sprintf("%s@channel%d", d.Event.Type, d.ChannelID))
	}
	return out
}

// sentEventType is the type of the Tend event m reports, as its webhook body
// carries it.
func sentEventType(m kitnotify.Message) string {
	if w, ok := m.Webhook.(event.TendWebhook); ok && w.Event != nil {
		return w.Event.Type
	}
	return ""
}

// orderedProvider fails its first send and records the type of every event it
// is asked to send, successful or not.
type orderedProvider struct {
	mu    sync.Mutex
	sends []string
	fails int
}

func (p *orderedProvider) Send(_ context.Context, m kitnotify.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sends = append(p.sends, sentEventType(m))
	if p.fails > 0 {
		p.fails--
		return errDestinationDown
	}
	return nil
}

var errDestinationDown = errors.New("destination down")

// TestWorkerDeliversAMissBeforeItsRecovery is the outage scenario end to end:
// heartbeat.missed and then heartbeat.recovered are queued for one channel,
// and the first send of the missed alert fails. The recovered alert must not
// go out until the missed one has.
func TestWorkerDeliversAMissBeforeItsRecovery(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		box := testBox(t)
		chID, err := notify.CreateChannel(ctx, s, box, notify.Channel{OrgID: org.ID, Name: "ops", Kind: kitnotify.Webhook}, []byte(`{"url":"https://example.invalid/x"}`))
		if err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		for _, ev := range []string{"heartbeat.missed", "heartbeat.recovered"} {
			if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: chID, EventType: ev, Enabled: true}); err != nil {
				t.Fatalf("CreateRule: %v", err)
			}
		}
		for _, ev := range []core.Event{heartbeat.MissedEvent(org.ID, "nightly"), heartbeat.RecoveredEvent(org.ID, "nightly")} {
			if _, err := s.EmitEvent(ctx, ev); err != nil {
				t.Fatalf("EmitEvent: %v", err)
			}
		}

		p := &orderedProvider{fails: 1}
		w := notify.NewWorker(s, box, func(kitnotify.ChannelType, []byte) (kitnotify.Provider, error) { return p, nil },
			"primary", notify.ChatOptions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if _, err := w.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		// The miss failed and backs off for a second; the recovery waits.
		if got := p.sends; len(got) != 1 || got[0] != "heartbeat.missed" {
			t.Fatalf("sends after the first pass = %v, want only the failed heartbeat.missed", got)
		}

		time.Sleep(1100 * time.Millisecond) // the first backoff is one second
		if n, err := w.DrainOnce(ctx); err != nil || n != 2 {
			t.Fatalf("second pass delivered %d (err %v), want 2", n, err)
		}
		want := []string{"heartbeat.missed", "heartbeat.missed", "heartbeat.recovered"}
		if got := p.sends; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("sends = %v, want %v", got, want)
		}
	})
}
