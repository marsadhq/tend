package notify

import (
	"fmt"
	"strings"
	"time"

	kitnotify "github.com/marsadhq/kit/notify"
)

// Pacing is how often a channel hears that a job is failing. A job's
// failures come in streaks (jobs.Streak: the runs in a row that did not
// succeed). A channel is sent the first failure of a streak, then a
// reminder at most once per its rule's interval (Rule.Repeat) while the
// streak lasts, and the run.recovered that ends it if it was sent one of its
// failures. The decisions are the functions below; the store applies them in
// the transaction that records each event and keeps an AlertState per
// channel and job.
//
//   - The first failure is sent unless the channel was sent one of the job's
//     failures less than its hold ago (the job recovered and failed again
//     soon after): then it is held, and sent when the hold ends if the job is
//     still failing (ReleaseDue). The hold is the interval, but at most an
//     hour (flapHold), so a long interval (a daily reminder) does not keep a
//     new outage quiet for a day; a flapping job sends at most one failure
//     and one recovery per hold, and a failing one is never left unannounced
//     for longer than it.
//   - The reminders fall on the interval marks after the alert that told the
//     channel of the streak (its anchor): each is the first failure at or
//     after its mark, or up to a minute (a tenth of a shorter interval)
//     before it, so a job that runs exactly as often as the interval reminds
//     on every failure although its runs end a little earlier or later each
//     time. Two are never less than an interval (less that slack) apart: a
//     reminder that went out late moves the next one to the mark after. The
//     marks are counted from the anchor with the rule's interval as it is
//     now, so a changed interval applies from the next failure.
//   - An interval of 0 sends every failure, as Tend always did.

// DefaultRepeat is the reminder interval of a rule that sets none, on a
// channel a person reads (Slack, Discord, email, Telegram, ntfy, Pushover):
// an hour, the re-alert gap Ward uses too. A webhook's default is every
// failure (EffectiveRepeat).
const DefaultRepeat = time.Hour

// MinRepeat and MaxRepeat bound the interval a rule can set (other than 0,
// every failure): below a minute every failure is meant, and a week is the
// longest reminder that is still one.
const (
	MinRepeat = time.Minute
	MaxRepeat = 7 * 24 * time.Hour
)

// EffectiveRepeat is the reminder interval of a rule that sets repeat (nil:
// none) on a channel of kind: what it sets, or by default every failure (0)
// for a webhook, whose receiver groups and paces alerts itself (Ward makes
// one incident of a job's failures and re-alerts on its own schedule, which
// it bases on the failures it receives), and DefaultRepeat for the others.
func EffectiveRepeat(kind kitnotify.ChannelType, repeat *time.Duration) time.Duration {
	if repeat != nil {
		return *repeat
	}
	if kind == kitnotify.Webhook {
		return 0
	}
	return DefaultRepeat
}

// ParseRepeat reads a rule's reminder interval as tend.yml and `tend rule
// add -repeat` write it: "every" (or 0) for every failure, or a Go duration
// from MinRepeat to MaxRepeat ("30m", "1h", "168h").
func ParseRepeat(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "every" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("repeat %q: want \"every\" or a duration such as 30m or 2h", s)
	}
	if d == 0 {
		return 0, nil
	}
	if d < MinRepeat || d > MaxRepeat {
		return 0, fmt.Errorf("repeat %q: want \"every\" or a duration from %s to %s", s, FormatRepeat(MinRepeat), FormatRepeat(MaxRepeat))
	}
	return d, nil
}

// FormatRepeat is d as ParseRepeat reads it: "every" for 0, else the
// duration without its zero units ("1h", "1h30m", "30m").
func FormatRepeat(d time.Duration) string {
	if d <= 0 {
		return "every"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// AlertState is what is kept about the failure alerts of one job on one
// channel.
type AlertState struct {
	// LastSentAt is when the channel was last sent one of the job's
	// failures (zero: never).
	LastSentAt time.Time
	// Anchor is when the alert that told the channel of the current streak
	// (or the last one it was told of) was sent: its reminder marks are an
	// interval apart from it.
	Anchor time.Time
	// Held says a failure of the current streak, which the channel has not
	// heard of, waits for its interval to end (ReleaseDue).
	Held bool
}

// Told reports whether the channel was sent a failure of the streak that
// began at since.
func (s AlertState) Told(since time.Time) bool {
	return !s.LastSentAt.IsZero() && !s.LastSentAt.Before(since)
}

// nextReminder is the mark the next reminder waits for under repeat: the
// first mark after the anchor, an interval apart, that is at least an
// interval (less slack) after the last alert.
func (s AlertState) nextReminder(repeat time.Duration) time.Time {
	anchor := s.Anchor
	if anchor.IsZero() || anchor.After(s.LastSentAt) {
		anchor = s.LastSentAt
	}
	earliest := s.LastSentAt.Add(repeat - slack(repeat))
	k := (earliest.Sub(anchor) + repeat - 1) / repeat
	return anchor.Add(max(k, 1) * repeat)
}

// flapHold is how long after a channel's last failure alert a failure that
// starts a new streak is held: the interval, but at most DefaultRepeat. The
// interval spaces reminders about one outage; a new outage is news, and an
// hour is as long as a flap is held back on a channel with the default.
func flapHold(repeat time.Duration) time.Duration {
	return min(repeat, DefaultRepeat)
}

// slack is how much earlier than a reminder mark a failure still counts as
// reaching it: a minute, or a tenth of an interval shorter than 10 minutes.
func slack(repeat time.Duration) time.Duration {
	return min(repeat/10, time.Minute)
}

// reached reports whether at is at mark, or within slack before it.
func reached(at, mark time.Time, repeat time.Duration) bool {
	return !at.Before(mark.Add(-slack(repeat)))
}

// PaceFailure decides whether a run.failed recorded at at, of a job failing
// since since, is sent to a channel whose interval is repeat and whose state
// is st (known false: the channel has none yet for the job), and returns the
// state that leaves.
func PaceFailure(st AlertState, known bool, at, since time.Time, repeat time.Duration) (send bool, next AlertState) {
	told := known && st.Told(since)
	switch {
	case repeat <= 0:
		next = AlertState{LastSentAt: at, Anchor: at}
		if told {
			next.Anchor = st.Anchor
		}
		return true, next
	case told:
		// A reminder, on its mark.
		if !reached(at, st.nextReminder(repeat), repeat) {
			st.Held = false
			return false, st
		}
		return true, AlertState{LastSentAt: at, Anchor: st.Anchor}
	case known && !reached(at, st.LastSentAt.Add(flapHold(repeat)), flapHold(repeat)):
		// A new streak soon after the last alert: held.
		st.Held = true
		return false, st
	default:
		// The first alert of the streak for this channel.
		return true, AlertState{LastSentAt: at, Anchor: at}
	}
}

// PaceRecovery reports whether the run.recovered that ends a streak begun at
// since is sent to a channel with state st: only when the channel was sent
// one of the streak's failures. A channel with no state for the job (known
// false: it does not get the job's failures, or got them before this was
// kept) and a streak whose start is not known (since zero) get it, as they
// always did.
func PaceRecovery(st AlertState, known bool, since time.Time) bool {
	return !known || since.IsZero() || st.Told(since)
}

// ReleaseDue reports whether a held failure (st.Held) is to be sent at now:
// the hold since the channel's last failure alert (flapHold) has ended.
func ReleaseDue(st AlertState, now time.Time, repeat time.Duration) bool {
	hold := flapHold(repeat)
	return st.Held && (repeat <= 0 || reached(now, st.LastSentAt.Add(hold), hold))
}

// Released is the state a channel is left in when its held failure is sent
// at now: told of the streak, its reminders an interval apart from now.
func Released(now time.Time) AlertState {
	return AlertState{LastSentAt: now, Anchor: now}
}
