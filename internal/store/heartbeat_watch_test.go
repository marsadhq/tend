package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/store"
)

// hbTSLayout mirrors the store's fixed-width storage layout so raw-seeded rows
// sort consistently with store-written rows.
const hbTSLayout = "2006-01-02T15:04:05.000000000Z07:00"

// seedWatchHeartbeat inserts a fully-specified heartbeat row via the raw DB and
// returns its row ID. lastSeen, when non-zero, is stored in the fixed-width
// layout; the zero value stores NULL.
func seedWatchHeartbeat(t *testing.T, ctx context.Context, s store.Store, backend string, orgID int64, name, token, status string, period, grace int, lastSeen time.Time) int64 {
	t.Helper()
	db := store.RawDB(s)
	if db == nil {
		t.Fatal("RawDB returned nil")
	}
	created := time.Now().UTC().Format(hbTSLayout)
	var ls any
	if !lastSeen.IsZero() {
		ls = lastSeen.UTC().Format(hbTSLayout)
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO heartbeats (org_id, name, token, period_seconds, grace_seconds, status, last_seen_at, created_at)
		 VALUES (`+phs(backend, 1, 8)+`)`,
		orgID, name, token, period, grace, status, ls, created)
	if err != nil {
		t.Fatalf("seed watch heartbeat %q: %v", name, err)
	}
	var id int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM heartbeats WHERE token = `+ph(backend, 1), token).Scan(&id); err != nil {
		t.Fatalf("read seeded heartbeat id: %v", err)
	}
	return id
}

func TestDueHeartbeats(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatalf("BootstrapDefaultOrg: %v", err)
			}

			// A: up, last_seen now-91s, period 60 grace 30 (deadline now-1s) -> DUE.
			idA := seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-a", "tok-a", "up", 60, 30, now.Add(-91*time.Second))
			// B: up, last_seen now-60s, period 60 grace 30 (deadline now+30s) -> NOT due.
			seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-b", "tok-b", "up", 60, 30, now.Add(-60*time.Second))
			// C: down, last_seen now-1000s -> NOT watched (excluded).
			seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-c", "tok-c", "down", 60, 30, now.Add(-1000*time.Second))
			// D: new, last_seen NULL -> NOT watched.
			seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-d", "tok-d", "new", 60, 30, time.Time{})

			due, err := s.DueHeartbeats(ctx, now)
			if err != nil {
				t.Fatalf("DueHeartbeats: %v", err)
			}
			if len(due) != 1 {
				names := make([]string, len(due))
				for i, hb := range due {
					names[i] = hb.Name
				}
				t.Fatalf("DueHeartbeats returned %d (%v), want exactly [hb-a]", len(due), names)
			}
			got := due[0]
			if got.ID != idA {
				t.Errorf("ID = %d, want %d", got.ID, idA)
			}
			if got.Name != "hb-a" {
				t.Errorf("Name = %q, want hb-a", got.Name)
			}
			if got.PeriodSeconds != 60 {
				t.Errorf("PeriodSeconds = %d, want 60", got.PeriodSeconds)
			}
			if got.GraceSeconds != 30 {
				t.Errorf("GraceSeconds = %d, want 30", got.GraceSeconds)
			}
			if got.Status != "up" {
				t.Errorf("Status = %q, want up", got.Status)
			}
			if got.Token != "tok-a" {
				t.Errorf("Token = %q, want tok-a", got.Token)
			}
			if got.OrgID != org.ID {
				t.Errorf("OrgID = %d, want %d", got.OrgID, org.ID)
			}
		})
	}
}

func TestSetHeartbeatStatus(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatalf("BootstrapDefaultOrg: %v", err)
			}
			id := seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-set", "tok-set", "up", 60, 30, now)

			if err := s.SetHeartbeatStatus(ctx, id, "down"); err != nil {
				t.Fatalf("SetHeartbeatStatus: %v", err)
			}

			var status string
			if err := store.RawDB(s).QueryRowContext(ctx,
				`SELECT status FROM heartbeats WHERE id = `+ph(b.name, 1), id).Scan(&status); err != nil {
				t.Fatalf("read status: %v", err)
			}
			if status != "down" {
				t.Fatalf("status = %q, want down", status)
			}
		})
	}
}

func TestSetHeartbeatStatusIf(t *testing.T) {
	ctx := context.Background()
	T := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	T2 := T.Add(10 * time.Second)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatalf("BootstrapDefaultOrg: %v", err)
			}

			// Sub-test 1: matching status+last_seen → fires; raw row shows 'down'.
			t.Run("fires_when_match", func(t *testing.T) {
				id := seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-if-match", "tok-if-match", "up", 60, 30, T)
				fired, err := s.SetHeartbeatStatusIf(ctx, id, "up", "down", T)
				if err != nil {
					t.Fatalf("SetHeartbeatStatusIf: %v", err)
				}
				if !fired {
					t.Fatal("fired=false; want true when status+last_seen match")
				}
				var status string
				if err := store.RawDB(s).QueryRowContext(ctx,
					`SELECT status FROM heartbeats WHERE id = `+ph(b.name, 1), id).Scan(&status); err != nil {
					t.Fatalf("read status: %v", err)
				}
				if status != "down" {
					t.Fatalf("status = %q, want down after successful transition", status)
				}
			})

			// Sub-test 2: stale last_seen_at (race case) → guard rejects; status stays 'up'.
			t.Run("rejects_stale_last_seen", func(t *testing.T) {
				id := seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-if-stale", "tok-if-stale", "up", 60, 30, T)
				// T2 != T - simulates a ping that re-stamped last_seen_at.
				fired, err := s.SetHeartbeatStatusIf(ctx, id, "up", "down", T2)
				if err != nil {
					t.Fatalf("SetHeartbeatStatusIf (stale): %v", err)
				}
				if fired {
					t.Fatal("fired=true for stale last_seen; want false (guard should reject)")
				}
				var status string
				if err := store.RawDB(s).QueryRowContext(ctx,
					`SELECT status FROM heartbeats WHERE id = `+ph(b.name, 1), id).Scan(&status); err != nil {
					t.Fatalf("read status: %v", err)
				}
				if status != "up" {
					t.Fatalf("status = %q, want up (guard kept it up)", status)
				}
			})

			// Sub-test 3: status guard - already 'down', so 'up'->'down' rejects.
			t.Run("rejects_wrong_status", func(t *testing.T) {
				id := seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-if-wrong-st", "tok-if-wrong-st", "up", 60, 30, T)
				// First call succeeds.
				if fired, err := s.SetHeartbeatStatusIf(ctx, id, "up", "down", T); err != nil || !fired {
					t.Fatalf("first SetHeartbeatStatusIf: fired=%v err=%v", fired, err)
				}
				// Second call: status is now 'down', so fromStatus='up' doesn't match.
				fired, err := s.SetHeartbeatStatusIf(ctx, id, "up", "down", T)
				if err != nil {
					t.Fatalf("second SetHeartbeatStatusIf: %v", err)
				}
				if fired {
					t.Fatal("fired=true on already-down heartbeat; want false (status guard)")
				}
			})
		})
	}
}

func TestDueHeartbeatsSkipsZeroPeriod(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	// An 'up' heartbeat last seen 3600s ago - well overdue if it were monitored.
	longAgo := now.Add(-3600 * time.Second)

	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatalf("BootstrapDefaultOrg: %v", err)
			}

			// period=0, grace=0: unconfigured → must NOT be returned by DueHeartbeats.
			seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-zero-period", "tok-zero-period", "up", 0, 0, longAgo)

			// period=60, grace=0: overdue (last seen 3600s ago) → MUST be returned.
			idConfigured := seedWatchHeartbeat(t, ctx, s, b.name, org.ID, "hb-configured", "tok-configured", "up", 60, 0, longAgo)

			due, err := s.DueHeartbeats(ctx, now)
			if err != nil {
				t.Fatalf("DueHeartbeats: %v", err)
			}
			// Only the configured heartbeat should appear.
			if len(due) != 1 {
				names := make([]string, len(due))
				for i, hb := range due {
					names[i] = hb.Name
				}
				t.Fatalf("DueHeartbeats returned %d (%v), want exactly [hb-configured]", len(due), names)
			}
			if due[0].ID != idConfigured {
				t.Errorf("ID = %d, want %d (hb-configured)", due[0].ID, idConfigured)
			}
		})
	}
}
