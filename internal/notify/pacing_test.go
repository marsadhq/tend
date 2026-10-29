package notify

import (
	"strings"
	"testing"
	"time"

	kitnotify "github.com/marsadhq/kit/notify"
)

var paceT0 = time.Date(2026, 10, 28, 19, 4, 0, 0, time.UTC)

// channelSim plays one channel's view of a job's runs through the pacing
// policy the way the store applies it, and counts what the channel is sent.
type channelSim struct {
	repeat  time.Duration
	st      AlertState
	known   bool
	since   time.Time // the current streak's start; zero while the job succeeds
	failed  []time.Time
	recover []time.Time
	held    bool
}

func (c *channelSim) fail(at time.Time) {
	if c.since.IsZero() {
		c.since = at
	}
	send, next := PaceFailure(c.st, c.known, at, c.since, c.repeat)
	c.st, c.known = next, true
	if send {
		c.failed = append(c.failed, at)
	}
}

func (c *channelSim) succeed(at time.Time) {
	if c.since.IsZero() {
		return // not a recovery
	}
	if PaceRecovery(c.st, c.known, c.since) {
		c.recover = append(c.recover, at)
	}
	c.st.Held = false
	c.since = time.Time{}
}

// tick is the delivery worker's poll: a held failure goes out once due.
func (c *channelSim) tick(now time.Time) {
	if c.known && ReleaseDue(c.st, now, c.repeat) {
		c.failed = append(c.failed, now)
		c.st = Released(now, c.repeat)
	}
}

// TestPacingTheIncident is the 2026-10-28 incident: a health check every 5
// minutes failed 216 times in a row over 18 hours, then succeeded. A chat
// channel (1h) hears the first failure, 17 hourly reminders and the
// recovery; a webhook (every failure) hears all 216 and the recovery, as it
// always has. A run ending a few seconds early or late does not move the
// reminders: they stay on the hour after the first alert.
func TestPacingTheIncident(t *testing.T) {
	for _, tc := range []struct {
		name          string
		repeat        time.Duration
		wantFailures  int
		wantRecovered int
	}{
		{"chat 1h", EffectiveRepeat(kitnotify.Telegram, nil), 18, 1},
		{"webhook every failure", EffectiveRepeat(kitnotify.Webhook, nil), 216, 1},
		{"30m", 30 * time.Minute, 36, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &channelSim{repeat: tc.repeat}
			for i := range 216 {
				jitter := time.Duration((i*7919)%21-10) * time.Second // -10s..+10s
				at := paceT0.Add(time.Duration(i)*5*time.Minute + jitter)
				c.tick(at)
				c.fail(at)
			}
			c.succeed(paceT0.Add(18 * time.Hour))
			if len(c.failed) != tc.wantFailures || len(c.recover) != tc.wantRecovered {
				t.Fatalf("sent %d failures and %d recoveries, want %d and %d", len(c.failed), len(c.recover), tc.wantFailures, tc.wantRecovered)
			}
			if tc.repeat == time.Hour {
				for i, at := range c.failed[1:] {
					mark := c.failed[0].Add(time.Duration(i+1) * time.Hour)
					if d := at.Sub(mark); d < -time.Minute || d > 5*time.Minute+10*time.Second {
						t.Errorf("reminder %d at %v, %v from its hour mark", i+1, at, d)
					}
				}
			}
		})
	}
}

// TestPacingAJobAsFrequentAsTheInterval: a job that runs exactly as often as
// the interval, whose runs end a little earlier or later each time, reminds
// on every failure rather than every other one.
func TestPacingAJobAsFrequentAsTheInterval(t *testing.T) {
	c := &channelSim{repeat: time.Hour}
	for i := range 10 {
		jitter := time.Duration([]int{0, -40, 30, -55, 10, -20, 50, -5, 25, -45}[i]) * time.Second
		c.fail(paceT0.Add(time.Duration(i)*time.Hour + jitter))
	}
	if len(c.failed) != 10 {
		t.Fatalf("an hourly job's 10 failures sent %d alerts, want 10", len(c.failed))
	}
}

// TestPacingAStreakResetsOnSuccess: a job that fails again long after it
// recovered starts over: its first failure is sent at once, and its
// reminders count from it.
func TestPacingAStreakResetsOnSuccess(t *testing.T) {
	c := &channelSim{repeat: time.Hour}
	for i := range 4 {
		c.fail(paceT0.Add(time.Duration(i) * 5 * time.Minute))
	}
	c.succeed(paceT0.Add(20 * time.Minute))
	again := paceT0.Add(3*time.Hour + 17*time.Minute)
	for i := range 13 {
		c.fail(again.Add(time.Duration(i) * 5 * time.Minute))
	}
	want := []time.Time{paceT0, again, again.Add(time.Hour)}
	if len(c.failed) != len(want) {
		t.Fatalf("sent failures at %v, want %v", c.failed, want)
	}
	for i := range want {
		if !c.failed[i].Equal(want[i]) {
			t.Errorf("failure %d sent at %v, want %v", i, c.failed[i], want[i])
		}
	}
	if len(c.recover) != 1 {
		t.Errorf("sent %d recoveries, want 1", len(c.recover))
	}
}

// TestPacingAFlappingJob: a job that fails and succeeds by turns every 5
// minutes for 3 hours sends one failure and one recovery an hour, not 36 of
// each: a failure within the interval of the last one sent is held, and the
// recovery of a failure the channel did not hear of is not sent.
func TestPacingAFlappingJob(t *testing.T) {
	c := &channelSim{repeat: time.Hour}
	for i := range 36 {
		at := paceT0.Add(time.Duration(i) * 5 * time.Minute)
		c.tick(at)
		if i%2 == 0 {
			c.fail(at)
		} else {
			c.succeed(at)
		}
	}
	if len(c.failed) != 3 || len(c.recover) != 3 {
		t.Fatalf("3 hours of flapping sent %d failures (%v) and %d recoveries, want 3 and 3", len(c.failed), c.failed, len(c.recover))
	}
	for i, at := range c.failed {
		if want := paceT0.Add(time.Duration(i) * time.Hour); !at.Equal(want) {
			t.Errorf("failure %d sent at %v, want %v", i, at, want)
		}
	}
}

// TestPacingAHeldFailureIsSentWhenItsIntervalEnds: a job that recovers and
// fails again soon after an alert, and then runs rarely, is not left looking
// healthy: the failure is held until the interval since the last alert ends,
// then sent by the worker's poll.
func TestPacingAHeldFailureIsSentWhenItsIntervalEnds(t *testing.T) {
	c := &channelSim{repeat: time.Hour}
	c.fail(paceT0)
	c.succeed(paceT0.Add(10 * time.Minute))
	c.fail(paceT0.Add(20 * time.Minute)) // held
	if len(c.failed) != 1 {
		t.Fatalf("the failure 20 minutes after an alert was sent at once")
	}
	c.tick(paceT0.Add(58 * time.Minute))
	if len(c.failed) != 1 {
		t.Fatalf("the held failure was sent before its interval ended")
	}
	c.tick(paceT0.Add(59*time.Minute + 30*time.Second))
	if len(c.failed) != 2 {
		t.Fatalf("the held failure was not sent when its interval ended")
	}
	c.tick(paceT0.Add(3 * time.Hour))
	if len(c.failed) != 2 {
		t.Fatalf("a released failure was sent again")
	}
	// Its reminders count from the release, and its recovery is sent.
	c.fail(paceT0.Add(time.Hour + 30*time.Minute))
	c.fail(paceT0.Add(2*time.Hour + time.Minute))
	c.succeed(paceT0.Add(2*time.Hour + 10*time.Minute))
	if len(c.failed) != 3 || len(c.recover) != 2 {
		t.Fatalf("sent %v and %d recoveries, want a reminder an hour after the release and the recovery", c.failed, len(c.recover))
	}
}

// TestPacingAHeldFailureThatRecoversIsDropped: a failure held back that
// recovers before its interval ends is never sent, and neither is its
// recovery.
func TestPacingAHeldFailureThatRecoversIsDropped(t *testing.T) {
	c := &channelSim{repeat: time.Hour}
	c.fail(paceT0)
	c.succeed(paceT0.Add(10 * time.Minute))
	c.fail(paceT0.Add(20 * time.Minute))
	c.succeed(paceT0.Add(25 * time.Minute))
	c.tick(paceT0.Add(2 * time.Hour))
	if len(c.failed) != 1 || len(c.recover) != 1 {
		t.Fatalf("sent %d failures and %d recoveries, want the first of each only", len(c.failed), len(c.recover))
	}
}

// TestPacingRecoveryWithoutAFailureRuleIsSent: a channel that routes
// run.recovered but has no record of the job's failures (it does not route
// run.failed, or the streak began before this version) hears every recovery,
// as it always has.
func TestPacingRecoveryWithoutAFailureRuleIsSent(t *testing.T) {
	if !PaceRecovery(AlertState{}, false, paceT0) {
		t.Error("a recovery for a channel without failure alerts was not sent")
	}
	if !PaceRecovery(AlertState{LastSentAt: paceT0.Add(-time.Hour)}, true, time.Time{}) {
		t.Error("a recovery of a streak of unknown start was not sent")
	}
	if PaceRecovery(AlertState{LastSentAt: paceT0.Add(-time.Minute)}, true, paceT0) {
		t.Error("a recovery the channel never heard the failure of was sent")
	}
}

// TestPacingEveryFailure: an interval of 0 sends every failure and keeps the
// record that makes its recovery go out.
func TestPacingEveryFailure(t *testing.T) {
	c := &channelSim{repeat: 0}
	for i := range 5 {
		c.fail(paceT0.Add(time.Duration(i) * time.Second))
	}
	c.succeed(paceT0.Add(time.Minute))
	if len(c.failed) != 5 || len(c.recover) != 1 {
		t.Fatalf("sent %d failures and %d recoveries, want 5 and 1", len(c.failed), len(c.recover))
	}
}

// TestPacingAChannelThatJoinsMidStreak: a channel whose rule is added while
// a job is failing (or the first alert after an upgrade) hears the next
// failure at once.
func TestPacingAChannelThatJoinsMidStreak(t *testing.T) {
	send, next := PaceFailure(AlertState{}, false, paceT0.Add(2*time.Hour), paceT0, time.Hour)
	if !send || !next.NextAt.Equal(paceT0.Add(3*time.Hour)) || !next.Told(paceT0) {
		t.Fatalf("PaceFailure for a new channel = %v, %+v", send, next)
	}
}

func TestEffectiveRepeat(t *testing.T) {
	half := 30 * time.Minute
	zero := time.Duration(0)
	for _, kind := range []kitnotify.ChannelType{kitnotify.Slack, kitnotify.Discord, kitnotify.SMTP, kitnotify.Telegram, kitnotify.Ntfy, kitnotify.Pushover} {
		if got := EffectiveRepeat(kind, nil); got != DefaultRepeat {
			t.Errorf("%s default = %v, want %v", kind, got, DefaultRepeat)
		}
		if got := EffectiveRepeat(kind, &half); got != half {
			t.Errorf("%s with 30m = %v", kind, got)
		}
	}
	if got := EffectiveRepeat(kitnotify.Webhook, nil); got != 0 {
		t.Errorf("webhook default = %v, want every failure", got)
	}
	if got := EffectiveRepeat(kitnotify.Webhook, &half); got != half {
		t.Errorf("webhook with 30m = %v", got)
	}
	if got := EffectiveRepeat(kitnotify.Slack, &zero); got != 0 {
		t.Errorf("slack with every = %v", got)
	}
}

func TestParseRepeat(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"every": 0, "0": 0, "0s": 0, "1h": time.Hour, "30m": 30 * time.Minute, "1m": time.Minute,
		"168h": 7 * 24 * time.Hour, "1h30m": 90 * time.Minute, " 2h ": 2 * time.Hour,
	} {
		got, err := ParseRepeat(in)
		if err != nil || got != want {
			t.Errorf("ParseRepeat(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "never", "1", "30s", "59s", "169h", "-1h", "1d"} {
		if got, err := ParseRepeat(in); err == nil {
			t.Errorf("ParseRepeat(%q) = %v, want an error", in, got)
		} else if !strings.Contains(err.Error(), "every") {
			t.Errorf("ParseRepeat(%q) error %q does not say what is accepted", in, err)
		}
	}
	for d, want := range map[time.Duration]string{0: "every", time.Hour: "1h", 90 * time.Minute: "1h30m", 30 * time.Minute: "30m", 168 * time.Hour: "168h"} {
		if got := FormatRepeat(d); got != want {
			t.Errorf("FormatRepeat(%v) = %q, want %q", d, got, want)
		}
		if back, err := ParseRepeat(FormatRepeat(d)); err != nil || back != d {
			t.Errorf("ParseRepeat(FormatRepeat(%v)) = %v, %v", d, back, err)
		}
	}
}
