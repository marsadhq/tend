package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// poisonProvider fails every send of one event type, the way a receiver
// refuses a body it cannot read, and records what it delivered.
type poisonProvider struct {
	mu        sync.Mutex
	poison    string
	permanent bool // a 400-style refusal rather than a 500-style one
	delivered []string
}

func (p *poisonProvider) Send(_ context.Context, m kitnotify.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sentEventType(m) == p.poison {
		err := errors.New("webhook POST ward.example: refused")
		if p.permanent {
			return kitnotify.Permanent(err)
		}
		return err
	}
	p.delivered = append(p.delivered, sentEventType(m))
	return nil
}

func (p *poisonProvider) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.delivered...)
}

// poisonedChannel sets up one channel with a run.failed queued ahead of a
// heartbeat.missed, and a worker whose provider is p.
func poisonedChannel(t *testing.T, ctx context.Context, s store.Store, p kitnotify.Provider) (orgID int64, w *notify.Worker) {
	t.Helper()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	box := testBox(t)
	chID, err := notify.CreateChannel(ctx, s, box, notify.Channel{OrgID: org.ID, Name: "ward", Kind: kitnotify.Webhook}, []byte(`{"url":"https://ward.example/x"}`))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	for _, ev := range []string{"run.failed", "heartbeat.missed"} {
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: org.ID, ChannelID: chID, EventType: ev, Enabled: true}); err != nil {
			t.Fatalf("CreateRule: %v", err)
		}
	}
	for _, ev := range []core.Event{
		{OrgID: org.ID, Type: "run.failed", Source: "jobs.runner", Payload: `{"job_id":1,"job_name":"nightly"}`},
		heartbeat.MissedEvent(org.ID, "backup"),
	} {
		if _, err := s.EmitEvent(ctx, ev); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}
	}
	w = notify.NewWorker(s, box, func(kitnotify.ChannelType, []byte) (kitnotify.Provider, error) { return p, nil },
		"primary", notify.ChatOptions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return org.ID, w
}

// TestRejectedHeadDoesNotBlockItsChannel: a delivery the receiver refuses for
// good (a 4xx) is given up on its first attempt, with notification.failed
// recorded, and the heartbeat.missed queued behind it goes out in the same
// pass. It used to be retried for 24 hours, holding the channel all along.
func TestRejectedHeadDoesNotBlockItsChannel(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := &poisonProvider{poison: "run.failed", permanent: true}
		orgID, w := poisonedChannel(t, ctx, s, p)

		if _, err := w.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		if got := p.sent(); len(got) != 1 || got[0] != "heartbeat.missed" {
			t.Fatalf("delivered %v in the first pass, want the heartbeat.missed behind the refused head", got)
		}
		failed := eventsOfType(t, ctx, s, orgID, "notification.failed")
		if len(failed) != 1 || failed[0].Payload != "run.failed" {
			t.Fatalf("notification.failed = %+v, want one for the refused run.failed", failed)
		}
		var pending int
		if err := store.RawDB(s).QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE state = 'pending' AND event_id IN (SELECT id FROM events WHERE type <> 'notification.failed')`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != 0 {
			t.Fatalf("%d of the channel's deliveries still pending, want none", pending)
		}
	})
}

// TestHeldChannelIsReleasedAfterTheHold is the bounded hold at claim level: a
// failing head holds back the deliveries behind it on its channel only until
// it was first attempted more than hold ago. After that they are claimed
// while the head keeps retrying, and ordering resumes among them.
func TestHeldChannelIsReleasedAfterTheHold(t *testing.T) {
	ctx := context.Background()
	const hold = 15 * time.Minute

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		seedAlertChannel(t, ctx, s, org.ID, "ward", "run.failed", "heartbeat.missed", "heartbeat.recovered")
		emit := func(ev core.Event) {
			t.Helper()
			if _, err := s.EmitEvent(ctx, ev); err != nil {
				t.Fatalf("EmitEvent: %v", err)
			}
		}
		emit(core.Event{OrgID: org.ID, Type: "run.failed", Source: "jobs.runner", Payload: `{"job_id":1}`}) // the head that keeps failing
		emit(heartbeat.MissedEvent(org.ID, "backup"))

		t0 := time.Now().Add(time.Second)
		claim := func(at time.Time) []notify.Delivery {
			t.Helper()
			ds, err := s.ClaimDueDeliveries(ctx, at, time.Minute, hold, 10)
			if err != nil {
				t.Fatalf("ClaimDueDeliveries: %v", err)
			}
			return ds
		}
		types := func(ds []notify.Delivery) []string {
			var out []string
			for _, d := range ds {
				out = append(out, d.Event.Type)
			}
			return out
		}

		first := claim(t0)
		if got := types(first); len(got) != 1 || got[0] != "run.failed" {
			t.Fatalf("first claim = %v, want only the head", got)
		}
		head := first[0].ID
		fail := func(at time.Time) {
			t.Helper()
			if err := s.RescheduleDelivery(ctx, head, at.Add(time.Second)); err != nil {
				t.Fatalf("RescheduleDelivery: %v", err)
			}
		}
		fail(t0)

		// Inside the hold the head retries alone.
		if got := types(claim(t0.Add(10 * time.Minute))); len(got) != 1 || got[0] != "run.failed" {
			t.Fatalf("claim inside the hold = %v, want only the head's retry", got)
		}
		fail(t0.Add(10 * time.Minute))

		// Past the hold the miss goes out beside the head's next retry.
		past := t0.Add(hold + time.Minute)
		got := claim(past)
		if ts := types(got); len(ts) != 2 || ts[0] != "run.failed" || ts[1] != "heartbeat.missed" {
			t.Fatalf("claim past the hold = %v, want the head's retry and the heartbeat.missed", ts)
		}
		missed := got[1].ID

		// Ordering resumes behind the released delivery: a recovery queued
		// after the miss waits for it, then goes even though the old head is
		// still pending.
		emit(heartbeat.RecoveredEvent(org.ID, 0, "backup"))
		if ts := types(claim(past)); len(ts) != 0 {
			t.Fatalf("claim while the miss is in flight = %v, want nothing", ts)
		}
		if err := s.MarkDeliveryDelivered(ctx, missed); err != nil {
			t.Fatal(err)
		}
		if ts := types(claim(past)); len(ts) != 1 || ts[0] != "heartbeat.recovered" {
			t.Fatalf("claim after the miss was delivered = %v, want the heartbeat.recovered", ts)
		}
	})
}

// TestZeroHoldTurnsOrderingOff: with hold 0 every due delivery is claimed.
func TestZeroHoldTurnsOrderingOff(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		seedAlertChannel(t, ctx, s, org.ID, "ward", "heartbeat.missed", "heartbeat.recovered")
		for _, ev := range []core.Event{heartbeat.MissedEvent(org.ID, "b"), heartbeat.RecoveredEvent(org.ID, 0, "b")} {
			if _, err := s.EmitEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
		}
		ds, err := s.ClaimDueDeliveries(ctx, time.Now().Add(time.Second), time.Minute, 0, 10)
		if err != nil || len(ds) != 2 {
			t.Fatalf("claim with hold 0 = %d deliveries (err %v), want both", len(ds), err)
		}
	})
}

// TestFailingHeadReleasesItsChannelWithinTheHold is the scenario end to end:
// a delivery the destination keeps failing (a 500, so it is retried) sits
// ahead of a heartbeat.missed. The miss must go out once the hold has passed,
// not after the head's 24-hour give-up, while the head keeps retrying.
func TestFailingHeadReleasesItsChannelWithinTheHold(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := &poisonProvider{poison: "run.failed"}
		_, w := poisonedChannel(t, ctx, s, p)
		w.Hold = 300 * time.Millisecond

		if _, err := w.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		if got := p.sent(); len(got) != 0 {
			t.Fatalf("delivered %v inside the hold, want nothing (the miss waits behind the head)", got)
		}

		time.Sleep(1100 * time.Millisecond) // past the hold and the head's first 1s backoff
		if _, err := w.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		if got := p.sent(); len(got) != 1 || got[0] != "heartbeat.missed" {
			t.Fatalf("delivered %v after the hold, want the heartbeat.missed", got)
		}
		var headState string
		var attempts int
		if err := store.RawDB(s).QueryRowContext(ctx,
			`SELECT d.state, d.attempts FROM deliveries d JOIN events e ON e.id = d.event_id WHERE e.type = 'run.failed'`).Scan(&headState, &attempts); err != nil {
			t.Fatal(err)
		}
		if headState != "pending" || attempts != 2 {
			t.Fatalf("head state=%s attempts=%d, want pending after 2 attempts (it keeps retrying)", headState, attempts)
		}
	})
}

// TestMigrationDatesAttemptedDeliveries: deliveries already attempted when
// migration 0007 runs count as first attempted at their creation, so they do
// not hold their channel for ever.
func TestMigrationDatesAttemptedDeliveries(t *testing.T) {
	ctx := context.Background()

	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seedAlertChannel(t, ctx, s, org.ID, "ward", "heartbeat.missed")
		for _, name := range []string{"tried", "untried"} {
			if _, err := s.EmitEvent(ctx, heartbeat.MissedEvent(org.ID, name)); err != nil {
				t.Fatal(err)
			}
		}
		db := store.RawDB(s)
		// As an older version left them: the first attempted, the second not.
		if _, err := db.ExecContext(ctx, `UPDATE deliveries SET attempts = 1 WHERE id = (SELECT MIN(id) FROM deliveries)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE deliveries DROP COLUMN first_attempt_at`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 7`); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		var dated, undated int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE first_attempt_at = created_at`).Scan(&dated); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE first_attempt_at IS NULL`).Scan(&undated); err != nil {
			t.Fatal(err)
		}
		if dated != 1 || undated != 1 {
			t.Fatalf("dated=%d undated=%d, want the attempted delivery dated from its creation and the other left unattempted", dated, undated)
		}
	})
}
