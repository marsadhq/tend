package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/kit/event"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
	"github.com/marsadhq/tend/internal/store"
)

// farFuture is a "now" at which every queued delivery is due.
var farFuture = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

// pacing is a store with one job ("backup") and the helpers the pacing tests
// share.
type pacing struct {
	t     *testing.T
	ctx   context.Context
	s     store.Store
	org   int64
	job   int64
	names map[int64]string
}

func newPacing(t *testing.T, s store.Store) *pacing {
	t.Helper()
	ctx := context.Background()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "backup", Type: jobs.Shell, Command: "false", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return &pacing{t: t, ctx: ctx, s: s, org: org.ID, job: job, names: map[int64]string{}}
}

// channel creates a channel of kind with an org-wide rule for each event,
// run.failed's with repeat (nil: the default).
func (p *pacing) channel(name string, kind kitnotify.ChannelType, repeat *time.Duration, events ...string) int64 {
	p.t.Helper()
	cfg, _ := json.Marshal(map[string]string{"url": "http://receiver.invalid/" + name, "chat_id": name})
	id, err := notify.CreateChannel(p.ctx, p.s, testBox(p.t), notify.Channel{OrgID: p.org, Name: name, Kind: kind}, cfg)
	if err != nil {
		p.t.Fatalf("CreateChannel %s: %v", name, err)
	}
	for _, ev := range events {
		p.rule(id, ev, 0, repeat)
	}
	p.names[id] = name
	return id
}

func (p *pacing) rule(ch int64, ev string, job int64, repeat *time.Duration) {
	p.t.Helper()
	r := notify.Rule{OrgID: p.org, ChannelID: ch, EventType: ev, JobID: job, Enabled: true}
	if ev == jobs.EventRunFailed {
		r.Repeat = repeat
	}
	if _, err := p.s.CreateRule(p.ctx, r); err != nil {
		p.t.Fatalf("CreateRule: %v", err)
	}
}

func (p *pacing) fail(at time.Time) int64 {
	p.t.Helper()
	return runAt(p.t, p.ctx, p.s, p.org, p.job, "runner", jobs.StatusFailed, at, "")
}

func (p *pacing) succeed(at time.Time) int64 {
	p.t.Helper()
	return runAt(p.t, p.ctx, p.s, p.org, p.job, "runner", jobs.StatusSucceeded, at, "")
}

// release is the delivery worker's poll at now.
func (p *pacing) release(now time.Time) int {
	p.t.Helper()
	n, err := p.s.ReleaseHeldAlerts(p.ctx, now)
	if err != nil {
		p.t.Fatalf("ReleaseHeldAlerts: %v", err)
	}
	return n
}

// sent claims every queued delivery, marks it delivered, and returns them by
// channel name, in order.
func (p *pacing) sent() map[string][]core.Event {
	p.t.Helper()
	out := map[string][]core.Event{}
	for {
		ds, err := p.s.ClaimDueDeliveries(p.ctx, farFuture, time.Minute, 0, 1000)
		if err != nil {
			p.t.Fatalf("ClaimDueDeliveries: %v", err)
		}
		if len(ds) == 0 {
			return out
		}
		// The claim returns its rows in no set order (Postgres); queued order
		// is id order.
		sort.Slice(ds, func(i, j int) bool { return ds[i].ID < ds[j].ID })
		for _, d := range ds {
			out[p.names[d.ChannelID]] = append(out[p.names[d.ChannelID]], d.Event)
			if err := p.s.MarkDeliveryDelivered(p.ctx, d.ID); err != nil {
				p.t.Fatal(err)
			}
		}
	}
}

// count is how many of evs are of type typ.
func count(evs []core.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func dur(d time.Duration) *time.Duration { return &d }

// recorder is a provider that keeps what it is sent.
type recorder struct {
	mu   *sync.Mutex
	name string
	got  map[string][]kitnotify.Message
}

func (r recorder) Send(_ context.Context, m kitnotify.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[r.name] = append(r.got[r.name], m)
	return nil
}

// fixedClock is a clock that says at.
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// TestPacingTheIncident replays the 2026-10-28 incident through the store,
// the delivery worker and the providers: a health check every 5 minutes
// fails 216 times in a row over 18 hours, then succeeds. The events table
// keeps all 216 failures. Telegram and Slack (the default hour) are each
// sent the first failure, 17 hourly reminders and the recovery: 19
// messages instead of 217. The webhook to Ward is sent every failure and the
// recovery, as before, each body byte for byte the shape of kit's fixture
// (the streak in the event's detail stays out of it).
func TestPacingTheIncident(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		var mu sync.Mutex
		var bodies [][]byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, b)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		both := []string{jobs.EventRunFailed, jobs.EventRunRecovered}
		p.channel("ops-telegram", kitnotify.Telegram, nil, both...)
		p.channel("ops-slack", kitnotify.Slack, nil, both...)
		cfg, _ := json.Marshal(map[string]string{"url": srv.URL})
		ward, err := notify.CreateChannel(p.ctx, s, testBox(t), notify.Channel{OrgID: p.org, Name: "ward", Kind: kitnotify.Webhook}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range both {
			p.rule(ward, ev, 0, nil)
		}

		got := map[string][]kitnotify.Message{}
		build := func(kind kitnotify.ChannelType, cfg []byte) (kitnotify.Provider, error) {
			if kind == kitnotify.Webhook {
				return kitnotify.BuildProvider(kind, cfg)
			}
			var c struct {
				ChatID string `json:"chat_id"`
			}
			_ = json.Unmarshal(cfg, &c)
			return recorder{mu: &mu, name: c.ChatID, got: got}, nil
		}
		w := notify.NewWorker(s, testBox(t), build, "primary", notify.ChatOptions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		w.Hold, w.Clock = 0, fixedClock{farFuture}

		for i := range 216 {
			jitter := time.Duration((i*7919)%21-10) * time.Second
			p.fail(streakT0.Add(time.Duration(i)*5*time.Minute + jitter))
			if _, err := w.DrainOnce(p.ctx); err != nil {
				t.Fatalf("DrainOnce: %v", err)
			}
		}
		p.succeed(streakT0.Add(18 * time.Hour))
		if _, err := w.DrainOnce(p.ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}

		if n := len(eventsOfType(t, p.ctx, s, p.org, jobs.EventRunFailed)); n != 216 {
			t.Errorf("events table has %d run.failed, want all 216", n)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, name := range []string{"ops-telegram", "ops-slack"} {
			if n := len(got[name]); n != 19 {
				t.Fatalf("%s was sent %d messages, want 19 (the failure, 17 reminders, the recovery)", name, n)
			}
		}
		// What Telegram showed: the first failure as always, then reminders
		// that say the job is still failing, then the recovery with the
		// streak it ended. (The first run ended 10 seconds before 19:04.)
		tg := got["ops-telegram"]
		for i, want := range []struct{ subject, body string }{
			{"Job failed: backup (exit 1)", ""},
			{"Job still failing: backup (exit 1)", "13 runs in a row have failed, since 2026-10-28 19:03 UTC (1h)."},
			{"Job still failing: backup (exit 1)", "25 runs in a row have failed, since 2026-10-28 19:03 UTC (2h)."},
		} {
			if tg[i].Subject != want.subject || tg[i].Body != want.body {
				t.Errorf("telegram message %d: %q / %q, want %q / %q", i, tg[i].Subject, tg[i].Body, want.subject, want.body)
			}
		}
		if last := tg[18]; last.Subject != "Job recovered: backup" ||
			last.Body != "It succeeded after 216 failed runs in a row, since 2026-10-28 19:03 UTC (18h)." {
			t.Errorf("telegram's last message: %q / %q", last.Subject, last.Body)
		}
		if len(bodies) != 217 {
			t.Fatalf("ward was sent %d bodies, want 217 (every failure and the recovery)", len(bodies))
		}
		failed, err := event.LoadTendFixture("run.failed", true)
		if err != nil {
			t.Fatal(err)
		}
		recovered, err := event.LoadTendFixture("run.recovered", true)
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range bodies {
			want := failed
			if i == 216 {
				want = recovered
			}
			if g, w := maskVolatile(b), maskVolatile(want); g != w {
				t.Fatalf("ward body %d differs from the fixture:\n got %s\nwant %s", i, g, w)
			}
		}
	})
}

// TestPacingSendsTheFirstFailureAndHourlyReminders checks what a chat
// channel is queued, and when, for the incident: the first failure, then the
// first failure at or after each hour since it, each with the streak so far
// in its detail.
func TestPacingSendsTheFirstFailureAndHourlyReminders(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		p.channel("ops-telegram", kitnotify.Telegram, nil, jobs.EventRunFailed, jobs.EventRunRecovered)
		for i := range 216 {
			p.fail(streakT0.Add(time.Duration(i) * 5 * time.Minute))
		}
		p.succeed(streakT0.Add(18 * time.Hour))
		evs := p.sent()["ops-telegram"]
		if len(evs) != 19 {
			t.Fatalf("queued %d, want 19", len(evs))
		}
		for k, e := range evs[:18] {
			if e.Type != jobs.EventRunFailed || !e.CreatedAt.Equal(streakT0.Add(time.Duration(k)*time.Hour)) {
				t.Errorf("message %d is %s at %v, want run.failed at %v", k, e.Type, e.CreatedAt, streakT0.Add(time.Duration(k)*time.Hour))
			}
			wantStreak(t, e, 12*k+1, streakT0)
		}
		if last := evs[18]; last.Type != jobs.EventRunRecovered {
			t.Errorf("last message is %s, want run.recovered", last.Type)
		} else {
			wantStreak(t, last, 216, streakT0)
		}
	})
}

// TestPacingResetsOnSuccess: once a job has succeeded, its next failure is a
// new streak and is sent at once, however soon after its last alert, if the
// recovery was the channel's last word on it... unless it is within the
// interval of the last failure alert (see TestPacingAFlappingJob). Here it
// fails again two hours later: sent at once, reminders an hour after that.
func TestPacingResetsOnSuccess(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		p.channel("ops-slack", kitnotify.Slack, nil, jobs.EventRunFailed, jobs.EventRunRecovered)
		p.fail(streakT0)
		p.fail(streakT0.Add(5 * time.Minute))
		p.succeed(streakT0.Add(10 * time.Minute))
		again := streakT0.Add(2*time.Hour + 7*time.Minute)
		for i := range 13 {
			p.fail(again.Add(time.Duration(i) * 5 * time.Minute))
		}
		evs := p.sent()["ops-slack"]
		var at []time.Time
		for _, e := range evs {
			at = append(at, e.CreatedAt)
		}
		want := []time.Time{streakT0, streakT0.Add(10 * time.Minute), again, again.Add(time.Hour)}
		if len(at) != len(want) {
			t.Fatalf("queued at %v, want %v", at, want)
		}
		for i := range want {
			if !at[i].Equal(want[i]) {
				t.Errorf("message %d at %v, want %v", i, at[i], want[i])
			}
		}
		wantStreak(t, evs[2], 1, again)
		wantStreak(t, evs[3], 13, again)
	})
}

// TestPacingAFlappingJob: a job that fails and succeeds by turns every 5
// minutes for 3 hours is recorded 18 times each way, but a chat channel is
// sent one failure and one recovery an hour; the webhook all 36.
func TestPacingAFlappingJob(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		p.channel("ops-telegram", kitnotify.Telegram, nil, jobs.EventRunFailed, jobs.EventRunRecovered)
		p.channel("ward", kitnotify.Webhook, nil, jobs.EventRunFailed, jobs.EventRunRecovered)
		for i := range 36 {
			at := streakT0.Add(time.Duration(i) * 5 * time.Minute)
			p.release(at)
			if i%2 == 0 {
				p.fail(at)
			} else {
				p.succeed(at)
			}
		}
		sent := p.sent()
		tg := sent["ops-telegram"]
		if count(tg, jobs.EventRunFailed) != 3 || count(tg, jobs.EventRunRecovered) != 3 {
			t.Errorf("telegram was sent %d failures and %d recoveries, want 3 and 3",
				count(tg, jobs.EventRunFailed), count(tg, jobs.EventRunRecovered))
		}
		for i, e := range tg {
			if want := streakT0.Add(time.Duration(i/2)*time.Hour + time.Duration(i%2)*5*time.Minute); !e.CreatedAt.Equal(want) {
				t.Errorf("message %d (%s) at %v, want %v", i, e.Type, e.CreatedAt, want)
			}
		}
		if ward := sent["ward"]; count(ward, jobs.EventRunFailed) != 18 || count(ward, jobs.EventRunRecovered) != 18 {
			t.Errorf("ward was sent %d failures and %d recoveries, want 18 and 18",
				count(ward, jobs.EventRunFailed), count(ward, jobs.EventRunRecovered))
		}
	})
}

// TestPacingReleasesAHeldFailure: a job that recovers and fails again soon
// after its alert, and then runs only rarely (a daily job run by hand), is
// not left looking healthy: its failure is held and sent by the worker's
// poll once the interval since the alert ends, its reminders count from
// then, and its recovery is sent. A held failure whose job recovers first is
// never sent, nor is that recovery.
func TestPacingReleasesAHeldFailure(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		p.channel("ops-telegram", kitnotify.Telegram, nil, jobs.EventRunFailed, jobs.EventRunRecovered)
		p.fail(streakT0)
		p.succeed(streakT0.Add(10 * time.Minute))
		held := runAt(t, p.ctx, s, p.org, p.job, jobs.ManualClaimant(), jobs.StatusFailed, streakT0.Add(20*time.Minute), "")
		if evs := p.sent()["ops-telegram"]; len(evs) != 2 {
			t.Fatalf("queued %d before the release, want the first failure and its recovery", len(evs))
		}
		if n := p.release(streakT0.Add(58 * time.Minute)); n != 0 {
			t.Fatalf("released %d before the interval ended", n)
		}
		if n := p.release(streakT0.Add(59*time.Minute + 30*time.Second)); n != 1 {
			t.Fatalf("released %d when the interval ended, want 1", n)
		}
		evs := p.sent()["ops-telegram"]
		if len(evs) != 1 || evs[0].DedupKey != fmt.Sprintf("run:%d:run.failed", held) {
			t.Fatalf("released %+v, want the held failure", evs)
		}
		if n := p.release(streakT0.Add(5 * time.Hour)); n != 0 {
			t.Fatalf("released the same failure again (%d)", n)
		}
		p.fail(streakT0.Add(time.Hour + 30*time.Minute)) // within the hour after the release
		p.fail(streakT0.Add(2*time.Hour + time.Minute))  // a reminder
		p.succeed(streakT0.Add(2*time.Hour + 5*time.Minute))
		evs = p.sent()["ops-telegram"]
		if len(evs) != 2 || evs[0].Type != jobs.EventRunFailed || evs[1].Type != jobs.EventRunRecovered {
			t.Fatalf("after the release queued %v, want a reminder and the recovery", types(evs))
		}

		// Held, then recovered before the interval ended: nothing.
		base := streakT0.Add(10 * time.Hour)
		p.fail(base)
		p.succeed(base.Add(5 * time.Minute))
		p.fail(base.Add(10 * time.Minute))
		p.succeed(base.Add(15 * time.Minute))
		p.release(base.Add(2 * time.Hour))
		if evs := p.sent()["ops-telegram"]; len(evs) != 2 {
			t.Fatalf("queued %v, want only the first failure and its recovery", types(evs))
		}
	})
}

func types(evs []core.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type+"@"+e.CreatedAt.Format("15:04:05"))
	}
	return out
}

// TestPacingTimeoutsAndManualRuns: a run that timed out, one the reaper
// failed and one `tend run` ran are failures of the streak like any other,
// paced the same; a manual run that succeeds ends the streak and sends the
// recovery.
func TestPacingTimeoutsAndManualRuns(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		p.channel("ops-slack", kitnotify.Slack, nil, jobs.EventRunFailed, jobs.EventRunRecovered)
		runAt(t, p.ctx, s, p.org, p.job, "runner", jobs.StatusTimedOut, streakT0, `{"timeout_seconds":30}`)
		runAt(t, p.ctx, s, p.org, p.job, jobs.ManualClaimant(), jobs.StatusFailed, streakT0.Add(10*time.Minute), "")
		reapAt(t, p.ctx, s, p.org, p.job, streakT0.Add(20*time.Minute))
		runAt(t, p.ctx, s, p.org, p.job, "runner", jobs.StatusTimedOut, streakT0.Add(time.Hour), `{"timeout_seconds":30}`)
		runAt(t, p.ctx, s, p.org, p.job, jobs.ManualClaimant(), jobs.StatusSucceeded, streakT0.Add(70*time.Minute), "")
		evs := p.sent()["ops-slack"]
		if got := types(evs); len(evs) != 3 || evs[0].Type != jobs.EventRunFailed || evs[1].Type != jobs.EventRunFailed || evs[2].Type != jobs.EventRunRecovered {
			t.Fatalf("queued %v, want the first timeout, the reminder an hour later, the recovery", got)
		}
		wantStreak(t, evs[1], 4, streakT0)
		wantStreak(t, evs[2], 4, streakT0)
	})
}

// TestPacingFollowsTheRules: the interval is set per rule, so per channel; a
// webhook can be paced and a chat channel sent every failure; where rules
// for one channel overlap (org-wide and scoped to the job) the most frequent
// wins, since each would send what it sends; and a channel that routes
// run.recovered but not run.failed hears every recovery, as it always has.
func TestPacingFollowsTheRules(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		p.channel("slack-every", kitnotify.Slack, dur(0), jobs.EventRunFailed)
		p.channel("ward-30m", kitnotify.Webhook, dur(30*time.Minute), jobs.EventRunFailed)
		overlap := p.channel("telegram-overlap", kitnotify.Telegram, nil, jobs.EventRunFailed)
		p.rule(overlap, jobs.EventRunFailed, p.job, dur(15*time.Minute))
		p.channel("recoveries-only", kitnotify.Ntfy, nil, jobs.EventRunRecovered)
		for i := range 12 { // an hour, every 5 minutes
			p.fail(streakT0.Add(time.Duration(i) * 5 * time.Minute))
		}
		p.succeed(streakT0.Add(time.Hour))
		sent := p.sent()
		for name, want := range map[string]int{"slack-every": 12, "ward-30m": 2, "telegram-overlap": 4, "recoveries-only": 0} {
			if got := count(sent[name], jobs.EventRunFailed); got != want {
				t.Errorf("%s was sent %d failures, want %d", name, got, want)
			}
		}
		if got := count(sent["recoveries-only"], jobs.EventRunRecovered); got != 1 {
			t.Errorf("recoveries-only was sent %d recoveries, want 1", got)
		}
	})
}

// TestPacingStateSurvivesARestart: what each channel was told lives in the
// database, so a Tend restarted in the middle of a streak, or while a
// failure is held, carries on as if it had not stopped.
func TestPacingStateSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	for _, db := range reopenables(t) {
		t.Run(db.name, func(t *testing.T) {
			s := db.open(t)
			p := newPacing(t, s)
			p.channel("ops-telegram", kitnotify.Telegram, nil, jobs.EventRunFailed, jobs.EventRunRecovered)
			for i := range 6 {
				p.fail(streakT0.Add(time.Duration(i) * 5 * time.Minute))
			}
			if evs := p.sent()["ops-telegram"]; len(evs) != 1 {
				t.Fatalf("queued %d before the restart, want 1", len(evs))
			}
			closeStore(s)

			s = db.open(t)
			defer closeStore(s)
			p.s = s
			for i := 6; i < 13; i++ {
				p.fail(streakT0.Add(time.Duration(i) * 5 * time.Minute))
			}
			evs := p.sent()["ops-telegram"]
			if len(evs) != 1 || !evs[0].CreatedAt.Equal(streakT0.Add(time.Hour)) {
				t.Fatalf("after the restart queued %v, want only the reminder at the hour", types(evs))
			}
			// A failure held across a restart is released after it.
			p.succeed(streakT0.Add(65 * time.Minute))
			p.fail(streakT0.Add(70 * time.Minute))
			closeStore(s)
			s = db.open(t)
			defer closeStore(s)
			p.s = s
			if n, err := s.ReleaseHeldAlerts(ctx, streakT0.Add(2*time.Hour)); err != nil || n != 1 {
				t.Fatalf("ReleaseHeldAlerts after a restart = %d, %v; want 1", n, err)
			}
		})
	}
}

// TestPacingStateGoesWithItsJobAndChannel: deleting a job or a channel
// deletes what was kept about its alerts, a held failure included, and a
// held failure whose channel no longer routes run.failed for the job is
// dropped. The job's removal is sent as before.
func TestPacingStateGoesWithItsJobAndChannel(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		raw := store.RawDB(s)
		rows := func() int {
			t.Helper()
			var n int
			if err := raw.QueryRowContext(p.ctx, `SELECT COUNT(*) FROM failure_alerts`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		tg := p.channel("ops-telegram", kitnotify.Telegram, nil, jobs.EventRunFailed, jobs.EventRunRecovered, jobs.EventJobRemoved)
		p.channel("ops-slack", kitnotify.Slack, nil, jobs.EventRunFailed, jobs.EventRunRecovered, jobs.EventJobRemoved)
		hold := func() {
			t.Helper()
			p.fail(streakT0)
			p.succeed(streakT0.Add(5 * time.Minute))
			p.fail(streakT0.Add(10 * time.Minute)) // held on both channels
			p.sent()
		}
		hold()
		if n := rows(); n != 2 {
			t.Fatalf("%d failure_alerts rows, want one per channel", n)
		}
		// The channel's rule goes: its held failure is dropped, not sent.
		rules, err := s.ListRules(p.ctx, p.org)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			if r.ChannelID == tg && r.EventType == jobs.EventRunFailed {
				if err := s.DeleteRule(p.ctx, p.org, r.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
		if n := p.release(streakT0.Add(2 * time.Hour)); n != 1 {
			t.Fatalf("released %d, want only the slack channel's", n)
		}
		p.sent()

		if err := s.DeleteChannel(p.ctx, p.org, tg); err != nil {
			t.Fatal(err)
		}
		if n := rows(); n != 1 {
			t.Fatalf("%d failure_alerts rows after the channel went, want 1", n)
		}
		if err := s.DeleteJob(p.ctx, p.org, p.job); err != nil {
			t.Fatal(err)
		}
		if n := rows(); n != 0 {
			t.Fatalf("%d failure_alerts rows after the job went, want none", n)
		}
		if evs := p.sent()["ops-slack"]; count(evs, jobs.EventJobRemoved) != 1 {
			t.Errorf("the job's removal was not sent: %v", types(evs))
		}
	})
}

// TestPacingKeepsAHeldEventFromPruning: the event of a held failure is not
// pruned before it is sent, however old.
func TestPacingKeepsAHeldEventFromPruning(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		p.channel("ops-telegram", kitnotify.Telegram, dur(48*time.Hour), jobs.EventRunFailed, jobs.EventRunRecovered)
		p.fail(streakT0)
		p.succeed(streakT0.Add(time.Minute))
		p.fail(streakT0.Add(2 * time.Minute))
		p.sent()
		if _, err := s.PruneEvents(p.ctx, farFuture); err != nil {
			t.Fatal(err)
		}
		if n := p.release(streakT0.Add(48 * time.Hour)); n != 1 {
			t.Fatalf("released %d, want the held failure", n)
		}
		if evs := p.sent()["ops-telegram"]; len(evs) != 1 || evs[0].Type != jobs.EventRunFailed {
			t.Fatalf("sent %v, want the held failure", types(evs))
		}
	})
}

// TestPacingFollowsARuleChange: a rule's new interval applies from the next
// failure (or poll), measured from the channel's last alert, also when it is
// shorter than the one in force at that alert.
func TestPacingFollowsARuleChange(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		p := newPacing(t, s)
		ch := p.channel("ops-telegram", kitnotify.Telegram, dur(4*time.Hour), jobs.EventRunFailed, jobs.EventRunRecovered)
		p.fail(streakT0)
		p.fail(streakT0.Add(30 * time.Minute))
		p.rule(ch, jobs.EventRunFailed, 0, dur(time.Hour))
		p.fail(streakT0.Add(time.Hour))
		evs := p.sent()["ops-telegram"]
		if len(evs) != 2 || !evs[1].CreatedAt.Equal(streakT0.Add(time.Hour)) {
			t.Fatalf("queued %v, want the first failure and one an hour later", types(evs))
		}

		// A failure held under a long interval is released an hour after the
		// alert once the rule says an hour.
		p.succeed(streakT0.Add(65 * time.Minute))
		slack := p.channel("ops-slack", kitnotify.Slack, dur(4*time.Hour), jobs.EventRunFailed, jobs.EventRunRecovered)
		p.fail(streakT0.Add(3 * time.Hour))
		p.succeed(streakT0.Add(3*time.Hour + 5*time.Minute))
		p.fail(streakT0.Add(3*time.Hour + 10*time.Minute)) // held on both: telegram until 4h, slack until 7h
		p.sent()
		p.rule(slack, jobs.EventRunFailed, 0, dur(time.Hour))
		if n := p.release(streakT0.Add(3*time.Hour + 59*time.Minute + 30*time.Second)); n != 2 {
			t.Fatalf("released %d at 4h, want both: telegram's mark, and slack's an hour after its alert under the shortened rule", n)
		}
	})
}

// TestPacingDropsAHeldFailureWhoseJobSucceeded: a held failure is sent only
// while the job is still failing by its runs, not just by its stored
// streak: a success recorded by v0.6 after a rollback leaves the streak and
// the hold as they were, and the release must not report a healthy job.
func TestPacingDropsAHeldFailureWhoseJobSucceeded(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := newPacing(t, b.store)
			p.channel("ops-telegram", kitnotify.Telegram, nil, jobs.EventRunFailed, jobs.EventRunRecovered)
			p.fail(streakT0)
			p.succeed(streakT0.Add(5 * time.Minute))
			p.fail(streakT0.Add(10 * time.Minute)) // held
			p.sent()
			now := store.FormatTime(time.Now())
			if _, err := store.RawDB(b.store).ExecContext(p.ctx, `INSERT INTO job_runs (org_id, job_id, status, attempt, exit_code, created_at, started_at, ended_at)
				VALUES (`+phs(b.name, 1, 8)+`)`, p.org, p.job, "succeeded", 1, 0, now, now, now); err != nil {
				t.Fatal(err)
			}
			if n := p.release(streakT0.Add(2 * time.Hour)); n != 0 {
				t.Fatalf("released %d for a job whose last run succeeded, want 0", n)
			}
			if evs := p.sent()["ops-telegram"]; len(evs) != 0 {
				t.Fatalf("sent %v", types(evs))
			}
		})
	}
}
