package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/store"
)

// pingT0 is the base instant of the ping log tests.
var pingT0 = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

// newPingHeartbeat creates a heartbeat with token tok (status 'new') and
// returns its org and row ID.
func newPingHeartbeat(t *testing.T, ctx context.Context, s store.Store, name, tok string) (orgID, id int64) {
	t.Helper()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	id, _, err = s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: name, Token: tok, PeriodSeconds: 3600, GraceSeconds: 300})
	if err != nil {
		t.Fatalf("CreateHeartbeat: %v", err)
	}
	return org.ID, id
}

// ping records one ping and fails the test on error.
func ping(t *testing.T, ctx context.Context, s store.Store, tok string, in heartbeat.PingInput, at time.Time) heartbeat.PingResult {
	t.Helper()
	res, err := s.RecordHeartbeatPing(ctx, tok, in, at)
	if err != nil {
		t.Fatalf("RecordHeartbeatPing(%s): %v", in.Kind, err)
	}
	return res
}

func getHB(t *testing.T, ctx context.Context, s store.Store, orgID, id int64) heartbeat.Heartbeat {
	t.Helper()
	hb, err := s.GetHeartbeat(ctx, orgID, id)
	if err != nil {
		t.Fatalf("GetHeartbeat: %v", err)
	}
	return hb
}

func pingLog(t *testing.T, ctx context.Context, s store.Store, orgID, id int64) []heartbeat.Ping {
	t.Helper()
	ps, err := s.ListHeartbeatPings(ctx, orgID, id, 100)
	if err != nil {
		t.Fatalf("ListHeartbeatPings: %v", err)
	}
	return ps
}

func exitStatus(n int) *int { return &n }

// TestPingKinds: every ping kind is logged, newest first, with what the
// client sent; success brings the heartbeat up, log changes nothing.
func TestPingKinds(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "backup", "tok-kinds")

		res := ping(t, ctx, s, "tok-kinds", heartbeat.PingInput{Kind: heartbeat.PingLog, Body: "just saying"}, pingT0)
		if res.Event != nil || res.HeartbeatID != id || res.OrgID != orgID {
			t.Fatalf("log ping result = %+v, want no event for heartbeat %d", res, id)
		}
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "new" || !hb.LastSeenAt.IsZero() {
			t.Fatalf("after /log: status %q last seen %v, want new and never", hb.Status, hb.LastSeenAt)
		}

		res = ping(t, ctx, s, "tok-kinds", heartbeat.PingInput{Kind: heartbeat.PingSuccess, ExitStatus: exitStatus(0), Body: "done"}, pingT0.Add(time.Second))
		if res.Event != nil || res.Name != "backup" {
			t.Fatalf("first success: result %+v, want no event (new is not a recovery)", res)
		}
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "up" || !hb.LastSeenAt.Equal(pingT0.Add(time.Second)) {
			t.Fatalf("after success: status %q last seen %v", hb.Status, hb.LastSeenAt)
		}

		ps := pingLog(t, ctx, s, orgID, id)
		if len(ps) != 2 {
			t.Fatalf("ping log has %d entries, want 2", len(ps))
		}
		if p := ps[0]; p.Kind != heartbeat.PingSuccess || p.ExitStatus == nil || *p.ExitStatus != 0 || p.Body != "done" ||
			!p.CreatedAt.Equal(pingT0.Add(time.Second)) || p.Duration != nil || p.HeartbeatID != id || p.OrgID != orgID {
			t.Fatalf("newest ping = %+v", p)
		}
		if p := ps[1]; p.Kind != heartbeat.PingLog || p.ExitStatus != nil || p.Body != "just saying" {
			t.Fatalf("older ping = %+v", p)
		}
	})
}

// TestFailurePingTakesTheHeartbeatDown: /fail and a non-zero exit status take
// the heartbeat down at once and record heartbeat.missed, with its delivery,
// carrying the exit status and output tail; a further failure while down
// records nothing more, and the next success is a recovery.
func TestFailurePingTakesTheHeartbeatDown(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "nightly", "tok-fail")
		chID := seedAlertChannel(t, ctx, s, orgID, "ops", "heartbeat.missed", "heartbeat.recovered")
		ping(t, ctx, s, "tok-fail", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, pingT0)

		res := ping(t, ctx, s, "tok-fail", heartbeat.PingInput{Kind: heartbeat.PingFail, ExitStatus: exitStatus(3), Body: "boom\n"}, pingT0.Add(time.Minute))
		if res.Event == nil || res.Event.Type != heartbeat.EventMissed || res.Event.Payload != "nightly" {
			t.Fatalf("failure result event = %+v, want heartbeat.missed for nightly", res.Event)
		}
		hb := getHB(t, ctx, s, orgID, id)
		if hb.Status != "down" || !hb.LastSeenAt.Equal(pingT0.Add(time.Minute)) {
			t.Fatalf("after failure: status %q last seen %v, want down at the failure", hb.Status, hb.LastSeenAt)
		}
		missed := eventsOfType(t, ctx, s, orgID, heartbeat.EventMissed)
		if len(missed) != 1 || missed[0].Payload != "nightly" {
			t.Fatalf("heartbeat.missed events = %+v, want one for nightly", missed)
		}
		d, ok := heartbeat.ParseMissDetail(missed[0].Detail)
		if !ok || d.Reason != heartbeat.ReasonFailed || d.ExitStatus == nil || *d.ExitStatus != 3 || d.Output != "boom" {
			t.Fatalf("stored detail %q = %+v", missed[0].Detail, d)
		}
		if n := pendingDeliveries(t, ctx, s, backendName(s), chID); n != 1 {
			t.Fatalf("pending deliveries = %d, want 1", n)
		}

		res = ping(t, ctx, s, "tok-fail", heartbeat.PingInput{Kind: heartbeat.PingFail}, pingT0.Add(2*time.Minute))
		if res.Event != nil {
			t.Fatalf("a failure while down recorded %+v, want nothing", res.Event)
		}
		if n := len(eventsOfType(t, ctx, s, orgID, heartbeat.EventMissed)); n != 1 {
			t.Fatalf("heartbeat.missed events = %d, want still 1", n)
		}

		res = ping(t, ctx, s, "tok-fail", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, pingT0.Add(3*time.Minute))
		if res.Event == nil || res.Event.Type != heartbeat.EventRecovered {
			t.Fatalf("success after failure: event %+v, want heartbeat.recovered", res.Event)
		}
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "up" {
			t.Fatalf("status after recovery = %q", hb.Status)
		}
		if n := len(eventsOfType(t, ctx, s, orgID, heartbeat.EventRecovered)); n != 1 {
			t.Fatalf("heartbeat.recovered events = %d, want 1", n)
		}

		ps := pingLog(t, ctx, s, orgID, id)
		if len(ps) != 4 || ps[1].Kind != heartbeat.PingFail || ps[1].ExitStatus != nil || ps[2].ExitStatus == nil || *ps[2].ExitStatus != 3 || ps[2].Body != "boom\n" {
			t.Fatalf("ping log = %+v", ps)
		}
	})
}

// TestFailurePingFromNew: a heartbeat that never succeeded also goes down on
// a failure.
func TestFailurePingFromNew(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "fresh", "tok-fresh-fail")
		res := ping(t, ctx, s, "tok-fresh-fail", heartbeat.PingInput{Kind: heartbeat.PingFail, ExitStatus: exitStatus(1)}, pingT0)
		if res.Event == nil || res.Event.Type != heartbeat.EventMissed {
			t.Fatalf("event = %+v, want heartbeat.missed", res.Event)
		}
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "down" {
			t.Fatalf("status = %q, want down", hb.Status)
		}
	})
}

// TestFailurePingIsAtomic: when the heartbeat.missed cannot be written, the
// heartbeat is not left down without it, and the ping is not logged.
func TestFailurePingIsAtomic(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "atomic", "tok-atomic")
		ping(t, ctx, s, "tok-atomic", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, pingT0)
		restore := breakEvents(t, ctx, s)
		if _, err := s.RecordHeartbeatPing(ctx, "tok-atomic", heartbeat.PingInput{Kind: heartbeat.PingFail}, pingT0.Add(time.Minute)); err == nil {
			t.Fatal("RecordHeartbeatPing succeeded with the events table gone")
		}
		restore()
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "up" || !hb.LastSeenAt.Equal(pingT0) {
			t.Fatalf("after a failed write: status %q last seen %v, want unchanged", hb.Status, hb.LastSeenAt)
		}
		if n := len(pingLog(t, ctx, s, orgID, id)); n != 1 {
			t.Fatalf("ping log has %d entries, want 1 (the failed one not logged)", n)
		}
	})
}

// TestStartAndCompletion: /start marks a run started without touching the
// status or last ping; the completion that follows records the run's
// duration and ends it.
func TestStartAndCompletion(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "timed", "tok-timed")
		ping(t, ctx, s, "tok-timed", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, pingT0)

		start := pingT0.Add(time.Hour)
		if res := ping(t, ctx, s, "tok-timed", heartbeat.PingInput{Kind: heartbeat.PingStart}, start); res.Event != nil {
			t.Fatalf("/start recorded %+v", res.Event)
		}
		hb := getHB(t, ctx, s, orgID, id)
		if hb.Status != "up" || !hb.LastSeenAt.Equal(pingT0) || !hb.StartedAt.Equal(start) || hb.StartedRID != "" {
			t.Fatalf("after /start: %+v", hb)
		}

		ping(t, ctx, s, "tok-timed", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, start.Add(90*time.Second))
		hb = getHB(t, ctx, s, orgID, id)
		if !hb.StartedAt.IsZero() || !hb.LastSeenAt.Equal(start.Add(90*time.Second)) {
			t.Fatalf("after completion: started %v last seen %v, want no run and the completion", hb.StartedAt, hb.LastSeenAt)
		}
		ps := pingLog(t, ctx, s, orgID, id)
		if ps[0].Duration == nil || *ps[0].Duration != 90*time.Second {
			t.Fatalf("completion duration = %v, want 1m30s", ps[0].Duration)
		}
		if ps[1].Kind != heartbeat.PingStart || ps[1].Duration != nil {
			t.Fatalf("start entry = %+v", ps[1])
		}

		// A second success with no start before it has no duration.
		ping(t, ctx, s, "tok-timed", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, start.Add(2*time.Minute))
		if d := pingLog(t, ctx, s, orgID, id)[0].Duration; d != nil {
			t.Fatalf("success without a start: duration %v, want none", *d)
		}

		// A completion more than 72 hours after its start is not paired.
		late := start.Add(time.Hour)
		ping(t, ctx, s, "tok-timed", heartbeat.PingInput{Kind: heartbeat.PingStart}, late)
		ping(t, ctx, s, "tok-timed", heartbeat.PingInput{Kind: heartbeat.PingFail}, late.Add(heartbeat.MaxRunDuration+time.Second))
		if d := pingLog(t, ctx, s, orgID, id)[0].Duration; d != nil {
			t.Fatalf("completion 72h+ after its start: duration %v, want none", *d)
		}
	})
}

// TestConcurrentRunsByRID: with ?rid= each completion is paired with its own
// start, and only the most recently started run is timed: an older run's
// completion leaves it running.
func TestConcurrentRunsByRID(t *testing.T) {
	ctx := context.Background()
	const ridA, ridB = "728b3763-ea80-4113-9fc0-f49b3adf226a", "0b2f8a8e-4a46-4b0e-9a1c-3f0e2d8f1c55"
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "parallel", "tok-rid")
		ping(t, ctx, s, "tok-rid", heartbeat.PingInput{Kind: heartbeat.PingStart, RID: ridA}, pingT0)
		ping(t, ctx, s, "tok-rid", heartbeat.PingInput{Kind: heartbeat.PingStart, RID: ridB}, pingT0.Add(10*time.Second))
		if hb := getHB(t, ctx, s, orgID, id); !hb.StartedAt.Equal(pingT0.Add(10*time.Second)) || hb.StartedRID != ridB {
			t.Fatalf("after two starts: started %v rid %q, want run B", hb.StartedAt, hb.StartedRID)
		}

		// A finishes first: paired with A's start, and B is still running.
		ping(t, ctx, s, "tok-rid", heartbeat.PingInput{Kind: heartbeat.PingSuccess, RID: ridA}, pingT0.Add(25*time.Second))
		if d := pingLog(t, ctx, s, orgID, id)[0].Duration; d == nil || *d != 25*time.Second {
			t.Fatalf("run A duration = %v, want 25s", d)
		}
		if hb := getHB(t, ctx, s, orgID, id); hb.StartedRID != ridB || hb.StartedAt.IsZero() {
			t.Fatalf("after A finished: started %v rid %q, want run B still running", hb.StartedAt, hb.StartedRID)
		}

		// B finishes: paired with B's start, and nothing is running.
		ping(t, ctx, s, "tok-rid", heartbeat.PingInput{Kind: heartbeat.PingSuccess, RID: ridB}, pingT0.Add(70*time.Second))
		if d := pingLog(t, ctx, s, orgID, id)[0].Duration; d == nil || *d != time.Minute {
			t.Fatalf("run B duration = %v, want 1m", d)
		}
		if hb := getHB(t, ctx, s, orgID, id); !hb.StartedAt.IsZero() || hb.StartedRID != "" {
			t.Fatalf("after B finished: started %v rid %q, want none", hb.StartedAt, hb.StartedRID)
		}

		// A completion whose rid matches no start has no duration.
		ping(t, ctx, s, "tok-rid", heartbeat.PingInput{Kind: heartbeat.PingSuccess, RID: "11111111-2222-4333-8444-555555555555"}, pingT0.Add(80*time.Second))
		if d := pingLog(t, ctx, s, orgID, id)[0].Duration; d != nil {
			t.Fatalf("unmatched rid: duration %v, want none", *d)
		}
		// Each start is paired once: a second completion with A's rid has none.
		ping(t, ctx, s, "tok-rid", heartbeat.PingInput{Kind: heartbeat.PingSuccess, RID: ridA}, pingT0.Add(90*time.Second))
		if d := pingLog(t, ctx, s, orgID, id)[0].Duration; d != nil {
			t.Fatalf("second completion of run A: duration %v, want none", *d)
		}
	})
}

// TestCompletionWithoutRIDOrFailureEndsTheRun: as in Healthchecks, a
// completion without a rid, and any failure, end the run that is being timed
// even when it was started with another rid; a success with another rid does
// not.
func TestCompletionWithoutRIDOrFailureEndsTheRun(t *testing.T) {
	ctx := context.Background()
	const rid, other = "728b3763-ea80-4113-9fc0-f49b3adf226a", "0b2f8a8e-4a46-4b0e-9a1c-3f0e2d8f1c55"
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "ends", "tok-ends")
		for i, c := range []struct {
			in      heartbeat.PingInput
			running bool
		}{
			{heartbeat.PingInput{Kind: heartbeat.PingSuccess, RID: other}, true},
			{heartbeat.PingInput{Kind: heartbeat.PingSuccess}, false},
			{heartbeat.PingInput{Kind: heartbeat.PingFail, RID: other}, false},
			{heartbeat.PingInput{Kind: heartbeat.PingLog, RID: rid}, true},
		} {
			at := pingT0.Add(time.Duration(i) * time.Hour)
			ping(t, ctx, s, "tok-ends", heartbeat.PingInput{Kind: heartbeat.PingStart, RID: rid}, at)
			ping(t, ctx, s, "tok-ends", c.in, at.Add(time.Minute))
			if hb := getHB(t, ctx, s, orgID, id); hb.StartedAt.IsZero() == c.running {
				t.Errorf("start then %s (rid %q): still running = %v, want %v", c.in.Kind, c.in.RID, !hb.StartedAt.IsZero(), c.running)
			}
		}
	})
}

// TestPingUnknownTokenWritesNothing: every kind answers ErrNotFound for a
// token no heartbeat owns.
func TestPingUnknownTokenWritesNothing(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "known", "tok-known-only")
		for _, kind := range []string{heartbeat.PingSuccess, heartbeat.PingStart, heartbeat.PingFail, heartbeat.PingLog} {
			if _, err := s.RecordHeartbeatPing(ctx, "no-such-token", heartbeat.PingInput{Kind: kind}, pingT0); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s with an unknown token: err = %v, want ErrNotFound", kind, err)
			}
		}
		if n := len(pingLog(t, ctx, s, orgID, id)); n != 0 {
			t.Fatalf("ping log has %d entries, want 0", n)
		}
		if _, err := s.RecordHeartbeatPing(ctx, "tok-known-only", heartbeat.PingInput{Kind: "bogus"}, pingT0); err == nil || errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown kind: err = %v, want a plain error", err)
		}
	})
}

// TestDeleteHeartbeatRemovesItsPings: the ping log goes with its heartbeat.
func TestDeleteHeartbeatRemovesItsPings(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "gone", "tok-gone-pings")
		ping(t, ctx, s, "tok-gone-pings", heartbeat.PingInput{Kind: heartbeat.PingLog, Body: "x"}, pingT0)
		if err := s.DeleteHeartbeat(ctx, orgID, id); err != nil {
			t.Fatalf("DeleteHeartbeat: %v", err)
		}
		var n int
		if err := store.RawDB(s).QueryRowContext(ctx, `SELECT COUNT(*) FROM heartbeat_pings`).Scan(&n); err != nil {
			t.Fatalf("count pings: %v", err)
		}
		if n != 0 {
			t.Fatalf("%d pings left after the heartbeat was deleted, want 0", n)
		}
	})
}

// TestListHeartbeatPingsIsOrgScoped: another org's id reads nothing.
func TestListHeartbeatPingsIsOrgScoped(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id := newPingHeartbeat(t, ctx, s, "mine", "tok-mine")
		ping(t, ctx, s, "tok-mine", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, pingT0)
		ps, err := s.ListHeartbeatPings(ctx, orgID+1, id, 10)
		if err != nil {
			t.Fatalf("ListHeartbeatPings: %v", err)
		}
		if len(ps) != 0 {
			t.Fatalf("foreign org read %d pings", len(ps))
		}
		if ps := pingLog(t, ctx, s, orgID, id); len(ps) != 1 {
			t.Fatalf("own org read %d pings, want 1", len(ps))
		}
	})
}
