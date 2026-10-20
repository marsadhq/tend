package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// seedAlertChannel creates a real channel plus an enabled org-wide rule
// routing evType to it, and returns the channel ID.
func seedAlertChannel(t *testing.T, ctx context.Context, s store.Store, orgID int64, name string, evTypes ...string) int64 {
	t.Helper()
	chID, err := s.CreateChannel(ctx, notify.Channel{OrgID: orgID, Name: name, Kind: notify.Webhook}, "blob")
	if err != nil {
		t.Fatalf("CreateChannel %q: %v", name, err)
	}
	for _, typ := range evTypes {
		if _, err := s.CreateRule(ctx, notify.Rule{OrgID: orgID, ChannelID: chID, EventType: typ, Enabled: true}); err != nil {
			t.Fatalf("CreateRule %s -> %q: %v", typ, name, err)
		}
	}
	return chID
}

// breakEvents makes every event insert fail until the returned func restores
// the table, so a test can see what a write that fails half-way leaves behind.
func breakEvents(t *testing.T, ctx context.Context, s store.Store) (restore func()) {
	t.Helper()
	db := store.RawDB(s)
	if _, err := db.ExecContext(ctx, `ALTER TABLE events RENAME TO events_broken`); err != nil {
		t.Fatalf("break events: %v", err)
	}
	restored := false
	restore = func() {
		if restored {
			return
		}
		restored = true
		if _, err := db.ExecContext(ctx, `ALTER TABLE events_broken RENAME TO events`); err != nil {
			t.Fatalf("restore events: %v", err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// eventsOfType returns the org's stored events of type typ.
func eventsOfType(t *testing.T, ctx context.Context, s store.Store, orgID int64, typ string) []core.Event {
	t.Helper()
	evs, err := s.ListEvents(ctx, orgID, 500)
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

// pendingDeliveries counts pending deliveries for channel chID.
func pendingDeliveries(t *testing.T, ctx context.Context, s store.Store, backend string, chID int64) int {
	t.Helper()
	var n int
	if err := store.RawDB(s).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deliveries WHERE state = 'pending' AND channel_id = `+ph(backend, 1), chID).Scan(&n); err != nil {
		t.Fatalf("count pending deliveries: %v", err)
	}
	return n
}

// heartbeatStatus reads a heartbeat's stored status.
func heartbeatStatus(t *testing.T, ctx context.Context, s store.Store, backend string, id int64) string {
	t.Helper()
	var status string
	if err := store.RawDB(s).QueryRowContext(ctx,
		`SELECT status FROM heartbeats WHERE id = `+ph(backend, 1), id).Scan(&status); err != nil {
		t.Fatalf("read heartbeat status: %v", err)
	}
	return status
}

// TestWatcherNeverMarksDownWithoutItsAlert pins that the down transition and
// the heartbeat.missed event (with its delivery) commit together. When the
// event cannot be written, the heartbeat must stay 'up' so the next Check
// tries again; it used to be left 'down' with no alert at all, and since a
// 'down' heartbeat is not watched, the miss was never reported.
func TestWatcherNeverMarksDownWithoutItsAlert(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatalf("BootstrapDefaultOrg: %v", err)
			}
			chID := seedAlertChannel(t, ctx, s, org.ID, "ops", "heartbeat.missed")
			id := seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "nightly", "tok-nightly", "up", 60, 30, now.Add(-time.Hour))

			w := heartbeat.NewWatcher(s, clock.NewFake(now), nil, nil)

			restore := breakEvents(t, ctx, s)
			if err := w.Check(ctx); err != nil {
				t.Fatalf("Check with events broken: %v", err)
			}
			if got := heartbeatStatus(t, ctx, s, b.name, id); got != "up" {
				t.Fatalf("status after a failed alert write = %q, want up (the miss must be retried, not lost)", got)
			}
			restore()

			if err := w.Check(ctx); err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got := heartbeatStatus(t, ctx, s, b.name, id); got != "down" {
				t.Fatalf("status = %q, want down", got)
			}
			if got := eventsOfType(t, ctx, s, org.ID, "heartbeat.missed"); len(got) != 1 || got[0].Payload != "nightly" {
				t.Fatalf("heartbeat.missed events = %+v, want exactly one for nightly", got)
			}
			if n := pendingDeliveries(t, ctx, s, b.name, chID); n != 1 {
				t.Fatalf("pending deliveries = %d, want 1", n)
			}

			// Already down: a further Check neither re-alerts nor errors.
			if err := w.Check(ctx); err != nil {
				t.Fatalf("Check (down): %v", err)
			}
			if got := eventsOfType(t, ctx, s, org.ID, "heartbeat.missed"); len(got) != 1 {
				t.Fatalf("heartbeat.missed events after a further Check = %d, want 1", len(got))
			}
		})
	}
}

// TestSetHeartbeatStatusIfAndEmitWritesNothingWhenGuardRejects pins that a
// transition the guard rejects (a ping won the race) records no event.
func TestSetHeartbeatStatusIfAndEmitWritesNothingWhenGuardRejects(t *testing.T) {
	ctx := context.Background()
	T := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatalf("BootstrapDefaultOrg: %v", err)
			}
			id := seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "raced", "tok-raced", "up", 60, 30, T)

			fired, err := s.SetHeartbeatStatusIfAndEmit(ctx, id, "up", "down", T.Add(time.Second), heartbeat.MissedEvent(org.ID, "raced"))
			if err != nil {
				t.Fatalf("SetHeartbeatStatusIfAndEmit: %v", err)
			}
			if fired {
				t.Fatal("fired = true for a stale last_seen_at, want false")
			}
			if got := heartbeatStatus(t, ctx, s, b.name, id); got != "up" {
				t.Fatalf("status = %q, want up", got)
			}
			if got := eventsOfType(t, ctx, s, org.ID, "heartbeat.missed"); len(got) != 0 {
				t.Fatalf("heartbeat.missed events = %d, want 0", len(got))
			}
		})
	}
}

// TestRecordPingRecordsItsRecovery pins that the ping that brings a down
// heartbeat back up also records heartbeat.recovered and its delivery, in the
// store and in the same transaction. The event used to be written by the HTTP
// handler afterwards, so it could be lost after the heartbeat was already up.
func TestRecordPingRecordsItsRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			orgID := seedHeartbeat(t, ctx, s, b.name, "nightly", "tok-down", "down")
			chID := seedAlertChannel(t, ctx, s, orgID, "ops", "heartbeat.recovered")

			gotOrg, name, recovered, err := s.RecordPing(ctx, "tok-down", now)
			if err != nil {
				t.Fatalf("RecordPing: %v", err)
			}
			if !recovered || name != "nightly" || gotOrg != orgID {
				t.Fatalf("RecordPing = (%d, %q, %v), want (%d, nightly, true)", gotOrg, name, recovered, orgID)
			}
			evs := eventsOfType(t, ctx, s, orgID, "heartbeat.recovered")
			if len(evs) != 1 {
				t.Fatalf("heartbeat.recovered events = %d, want 1", len(evs))
			}
			if evs[0].Payload != "nightly" || evs[0].Source != "heartbeat" || evs[0].DedupKey != "" {
				t.Fatalf("recovered event = %+v, want payload nightly, source heartbeat, no dedup key", evs[0])
			}
			if n := pendingDeliveries(t, ctx, s, b.name, chID); n != 1 {
				t.Fatalf("pending deliveries = %d, want 1", n)
			}

			// A second ping on the now-up heartbeat is not a recovery.
			if _, _, recovered, err := s.RecordPing(ctx, "tok-down", now.Add(time.Minute)); err != nil || recovered {
				t.Fatalf("second ping: recovered=%v err=%v, want false/nil", recovered, err)
			}
			if evs := eventsOfType(t, ctx, s, orgID, "heartbeat.recovered"); len(evs) != 1 {
				t.Fatalf("heartbeat.recovered events after a second ping = %d, want 1", len(evs))
			}
		})
	}
}

// TestRecordPingConcurrentRecoveryIsRecordedOnce fires many pings at one down
// heartbeat at once: exactly one of them may report the recovery, and exactly
// one heartbeat.recovered may be stored.
func TestRecordPingConcurrentRecoveryIsRecordedOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			orgID := seedHeartbeat(t, ctx, s, b.name, "fleet", "tok-fleet", "down")

			const pings = 16
			var (
				wg        sync.WaitGroup
				mu        sync.Mutex
				recovered int
				errs      []error
			)
			start := make(chan struct{})
			for i := 0; i < pings; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					_, _, rec, err := s.RecordPing(ctx, "tok-fleet", now.Add(time.Duration(i)*time.Millisecond))
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err)
					}
					if rec {
						recovered++
					}
				}(i)
			}
			close(start)
			wg.Wait()

			if len(errs) > 0 {
				t.Fatalf("RecordPing errors: %v", errs)
			}
			if recovered != 1 {
				t.Fatalf("pings reporting a recovery = %d, want exactly 1", recovered)
			}
			if evs := eventsOfType(t, ctx, s, orgID, "heartbeat.recovered"); len(evs) != 1 {
				t.Fatalf("heartbeat.recovered events = %d, want exactly 1", len(evs))
			}
		})
	}
}

// TestRecordPingRecoveryIsAllOrNothing pins that when the recovery event
// cannot be written the ping is not recorded either: the heartbeat stays down,
// and the next ping recovers it with its event.
func TestRecordPingRecoveryIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			orgID := seedHeartbeat(t, ctx, s, b.name, "nightly", "tok-aon", "down")

			restore := breakEvents(t, ctx, s)
			if _, _, _, err := s.RecordPing(ctx, "tok-aon", now); err == nil {
				t.Fatal("RecordPing with events broken: err = nil, want the write to fail")
			}
			if status, _ := readHeartbeat(t, ctx, s, b.name, "tok-aon"); status != "down" {
				t.Fatalf("status after a failed recovery = %q, want down", status)
			}
			restore()

			if _, _, recovered, err := s.RecordPing(ctx, "tok-aon", now); err != nil || !recovered {
				t.Fatalf("RecordPing: recovered=%v err=%v, want true/nil", recovered, err)
			}
			if evs := eventsOfType(t, ctx, s, orgID, "heartbeat.recovered"); len(evs) != 1 {
				t.Fatalf("heartbeat.recovered events = %d, want 1", len(evs))
			}
		})
	}
}
