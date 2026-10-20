package heartbeat_test

import (
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/heartbeat"
)

// TestDeadline pins when a heartbeat goes down: period + grace after its last
// ping (or creation, never pinged), brought forward to grace after a started
// run, never pushed back by one; down and period-less heartbeats are not
// watched.
func TestDeadline(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	hb := func(status string, lastSeen, started time.Time) heartbeat.Heartbeat {
		return heartbeat.Heartbeat{Status: status, PeriodSeconds: 3600, GraceSeconds: 300, CreatedAt: t0.Add(-time.Hour), LastSeenAt: lastSeen, StartedAt: started}
	}
	for _, c := range []struct {
		name    string
		hb      heartbeat.Heartbeat
		want    time.Time
		overran bool
		watched bool
	}{
		{"up", hb("up", t0, time.Time{}), t0.Add(65 * time.Minute), false, true},
		{"new counts from creation", hb("new", time.Time{}, time.Time{}), t0.Add(5 * time.Minute), false, true},
		{"started run", hb("up", t0, t0.Add(10*time.Minute)), t0.Add(15 * time.Minute), true, true},
		{"late start does not postpone", hb("up", t0, t0.Add(62*time.Minute)), t0.Add(65 * time.Minute), false, true},
		{"new with a start", hb("new", time.Time{}, t0.Add(-58*time.Minute)), t0.Add(-53 * time.Minute), true, true},
		{"down", hb("down", t0, time.Time{}), time.Time{}, false, false},
		{"no period", heartbeat.Heartbeat{Status: "up", LastSeenAt: t0}, time.Time{}, false, false},
	} {
		got, overran, watched := c.hb.Deadline()
		if !got.Equal(c.want) || overran != c.overran || watched != c.watched {
			t.Errorf("%s: Deadline() = %v, overran %v, watched %v; want %v, %v, %v", c.name, got, overran, watched, c.want, c.overran, c.watched)
		}
	}
}
