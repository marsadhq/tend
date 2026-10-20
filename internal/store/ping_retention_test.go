package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/store"
)

// TestPruneHeartbeatPings: the retention sweep removes ping log entries older
// than the cutoff and keeps newer ones.
func TestPruneHeartbeatPings(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "pruned", "tok-pruned")
		now := time.Now()
		ping(t, ctx, s, "tok-pruned", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, now.Add(-40*24*time.Hour))
		ping(t, ctx, s, "tok-pruned", heartbeat.PingInput{Kind: heartbeat.PingLog, Body: "recent"}, now.Add(-time.Hour))

		n, err := s.PruneHeartbeatPings(ctx, now.Add(-30*24*time.Hour))
		if err != nil {
			t.Fatalf("PruneHeartbeatPings: %v", err)
		}
		if n != 1 {
			t.Fatalf("pruned %d pings, want 1", n)
		}
		ps := pingLog(t, ctx, s, orgID, id)
		if len(ps) != 1 || ps[0].Body != "recent" {
			t.Fatalf("ping log after prune = %+v, want the recent entry only", ps)
		}
	})
}

// TestPingLogIsCappedPerHeartbeat: a heartbeat keeps only its newest
// MaxPingsPerHeartbeat entries, whatever their age, so a runaway client cannot
// fill the disk between two sweeps; other heartbeats are not touched.
func TestPingLogIsCappedPerHeartbeat(t *testing.T) {
	ctx := context.Background()
	defer store.SetMaxPingsPerHeartbeat(5)()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "chatty", "tok-chatty")
		if _, _, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: orgID, Name: "calm", Token: "tok-calm-cap", PeriodSeconds: 60}); err != nil {
			t.Fatalf("CreateHeartbeat: %v", err)
		}
		ping(t, ctx, s, "tok-calm-cap", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, pingT0)
		for i := 0; i < 8; i++ {
			ping(t, ctx, s, "tok-chatty", heartbeat.PingInput{Kind: heartbeat.PingLog, Body: string(rune('a' + i))}, pingT0.Add(time.Duration(i)*time.Second))
		}
		ps := pingLog(t, ctx, s, orgID, id)
		if len(ps) != 5 || ps[0].Body != "h" || ps[4].Body != "d" {
			t.Fatalf("capped log = %d entries (%+v), want the newest 5 (h..d)", len(ps), ps)
		}
		if n := countTable(t, s, "heartbeat_pings"); n != 6 {
			t.Fatalf("heartbeat_pings rows = %d, want 6 (5 + the other heartbeat's 1)", n)
		}
	})
}
