package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/store"
)

// stepClock is a clock a test sets.
type stepClock struct{ at time.Time }

func (c *stepClock) Now() time.Time { return c.at }

// TestHeartbeatAlertsOncePerOutage: unlike a job, a heartbeat records one
// heartbeat.missed when it goes down and nothing more until it recovers,
// however long it stays down and however often the watcher looks. Here a
// job reporting through `tend exec` every 5 minutes fails for 18 hours (a
// start and a failure ping each time, the watcher checking every 30
// seconds), and a heartbeat that stops pinging is checked every minute for
// 18 hours: one miss and one recovery each.
func TestHeartbeatAlertsOncePerOutage(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, hb := range []string{"exec-job", "silent"} {
			if _, _, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: hb, Token: "tok-" + hb, PeriodSeconds: 300, GraceSeconds: 60}); err != nil {
				t.Fatal(err)
			}
		}
		chID := seedAlertChannel(t, ctx, s, org.ID, "ops", heartbeat.EventMissed, heartbeat.EventRecovered)
		clk := &stepClock{at: pingT0}
		w := heartbeat.NewWatcher(s, clk, nil, nil)
		check := func(at time.Time) {
			t.Helper()
			clk.at = at
			if err := w.Check(ctx); err != nil {
				t.Fatalf("Check: %v", err)
			}
		}
		ping(t, ctx, s, "tok-exec-job", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, pingT0)
		ping(t, ctx, s, "tok-silent", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, pingT0)
		for i := 1; i <= 216; i++ {
			at := pingT0.Add(time.Duration(i) * 5 * time.Minute)
			ping(t, ctx, s, "tok-exec-job", heartbeat.PingInput{Kind: heartbeat.PingStart, RID: "r"}, at)
			ping(t, ctx, s, "tok-exec-job", heartbeat.PingInput{Kind: heartbeat.PingFail, RID: "r", ExitStatus: exitStatus(1), Body: "connection refused\n"}, at.Add(10*time.Second))
			for _, c := range []time.Duration{30 * time.Second, 60 * time.Second, 90 * time.Second, 120 * time.Second, 150 * time.Second, 180 * time.Second, 210 * time.Second, 240 * time.Second, 270 * time.Second, 300 * time.Second} {
				check(at.Add(c - 5*time.Second))
			}
		}
		end := pingT0.Add(217 * 5 * time.Minute)
		ping(t, ctx, s, "tok-exec-job", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, end)
		ping(t, ctx, s, "tok-silent", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, end)

		byName := map[string][2]int{}
		for _, e := range eventsOfType(t, ctx, s, org.ID, heartbeat.EventMissed) {
			n := byName[e.Payload]
			n[0]++
			byName[e.Payload] = n
		}
		for _, e := range eventsOfType(t, ctx, s, org.ID, heartbeat.EventRecovered) {
			n := byName[e.Payload]
			n[1]++
			byName[e.Payload] = n
		}
		for _, hb := range []string{"exec-job", "silent"} {
			if got := byName[hb]; got != [2]int{1, 1} {
				t.Errorf("%s recorded %d misses and %d recoveries over 18 hours down, want 1 and 1", hb, got[0], got[1])
			}
		}
		if n := pendingDeliveries(t, ctx, s, backendName(s), chID); n != 4 {
			t.Errorf("%d deliveries queued, want 4 (a miss and a recovery for each)", n)
		}
	})
}
