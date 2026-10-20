package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/store"
)

// overrunHeartbeat creates a heartbeat (period 1h, grace 5m) that last pinged
// at base, and a watcher on a fake clock at base.
func overrunHeartbeat(t *testing.T, ctx context.Context, s store.Store, name, tok string, base time.Time) (orgID, id int64, fk *clock.FakeClock, w *heartbeat.Watcher) {
	t.Helper()
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	id, _, err = s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: name, Token: tok, PeriodSeconds: 3600, GraceSeconds: 300})
	if err != nil {
		t.Fatalf("CreateHeartbeat: %v", err)
	}
	ping(t, ctx, s, tok, heartbeat.PingInput{Kind: heartbeat.PingSuccess}, base)
	fk = clock.NewFake(base)
	return org.ID, id, fk, heartbeat.NewWatcher(s, fk, nil, nil)
}

func checkAt(t *testing.T, ctx context.Context, w *heartbeat.Watcher, fk *clock.FakeClock, at time.Time) {
	t.Helper()
	fk.Advance(at.Sub(fk.Now()))
	if err := w.Check(ctx); err != nil {
		t.Fatalf("Check at %v: %v", at, err)
	}
}

// TestStartedRunThatOverrunsGoesDown: after /start, a heartbeat whose run
// does not finish within its grace goes down (Healthchecks semantics), with a
// heartbeat.missed saying the run did not finish, and the run is over.
func TestStartedRunThatOverrunsGoesDown(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id, fk, w := overrunHeartbeat(t, ctx, s, "sync", "tok-overrun", base)
		start := base.Add(10 * time.Minute)
		ping(t, ctx, s, "tok-overrun", heartbeat.PingInput{Kind: heartbeat.PingStart}, start)

		checkAt(t, ctx, w, fk, start.Add(5*time.Minute)) // exactly the grace: not yet
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "up" {
			t.Fatalf("at start+grace: status %q, want up", hb.Status)
		}
		checkAt(t, ctx, w, fk, start.Add(5*time.Minute+time.Second))
		hb := getHB(t, ctx, s, orgID, id)
		if hb.Status != "down" || !hb.StartedAt.IsZero() || hb.StartedRID != "" {
			t.Fatalf("past start+grace: %+v, want down with the run ended", hb)
		}
		missed := eventsOfType(t, ctx, s, orgID, heartbeat.EventMissed)
		if len(missed) != 1 || missed[0].Payload != "sync" {
			t.Fatalf("heartbeat.missed = %+v, want one for sync", missed)
		}
		d, ok := heartbeat.ParseMissDetail(missed[0].Detail)
		if !ok || d.Reason != heartbeat.ReasonOverran || !d.StartedAt.Equal(start) || d.GraceSeconds != 300 {
			t.Fatalf("detail %q = %+v, want overran from %v", missed[0].Detail, d, start)
		}

		// The next success recovers it, whatever its rid, and it stays up.
		ping(t, ctx, s, "tok-overrun", heartbeat.PingInput{Kind: heartbeat.PingSuccess, RID: "0b2f8a8e-4a46-4b0e-9a1c-3f0e2d8f1c55"}, start.Add(20*time.Minute))
		checkAt(t, ctx, w, fk, start.Add(21*time.Minute))
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "up" {
			t.Fatalf("after recovery: status %q, want up", hb.Status)
		}
		if n := len(eventsOfType(t, ctx, s, orgID, heartbeat.EventMissed)); n != 1 {
			t.Fatalf("heartbeat.missed events = %d, want still 1", n)
		}
	})
}

// TestStartedRunThatFinishesInTimeStaysUp: a completion within the grace ends
// the run, and the regular deadline applies again.
func TestStartedRunThatFinishesInTimeStaysUp(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id, fk, w := overrunHeartbeat(t, ctx, s, "quick", "tok-quick", base)
		start := base.Add(10 * time.Minute)
		ping(t, ctx, s, "tok-quick", heartbeat.PingInput{Kind: heartbeat.PingStart}, start)
		ping(t, ctx, s, "tok-quick", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, start.Add(4*time.Minute))

		checkAt(t, ctx, w, fk, start.Add(6*time.Minute))
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "up" {
			t.Fatalf("finished in time: status %q, want up", hb.Status)
		}
		// Regular deadline: last ping start+4m, period 1h, grace 5m.
		checkAt(t, ctx, w, fk, start.Add(4*time.Minute+65*time.Minute))
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "up" {
			t.Fatalf("at the regular deadline: status %q, want up", hb.Status)
		}
		checkAt(t, ctx, w, fk, start.Add(4*time.Minute+65*time.Minute+time.Second))
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "down" {
			t.Fatalf("past the regular deadline: status %q, want down", hb.Status)
		}
		missed := eventsOfType(t, ctx, s, orgID, heartbeat.EventMissed)
		if len(missed) != 1 || missed[0].Detail != "" {
			t.Fatalf("heartbeat.missed = %+v, want one plain miss", missed)
		}
	})
}

// TestStartDoesNotPostponeTheRegularDeadline: as in Healthchecks the deadline
// is the earlier of the regular one and start+grace, so a start late in the
// period cannot buy the heartbeat more time.
func TestStartDoesNotPostponeTheRegularDeadline(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id, fk, w := overrunHeartbeat(t, ctx, s, "late", "tok-late", base)
		// Regular deadline base+65m; a start at base+62m would end at base+67m.
		ping(t, ctx, s, "tok-late", heartbeat.PingInput{Kind: heartbeat.PingStart}, base.Add(62*time.Minute))
		checkAt(t, ctx, w, fk, base.Add(65*time.Minute+time.Second))
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "down" {
			t.Fatalf("past the regular deadline with a later start: status %q, want down", hb.Status)
		}
		missed := eventsOfType(t, ctx, s, orgID, heartbeat.EventMissed)
		if len(missed) != 1 || missed[0].Detail != "" {
			t.Fatalf("heartbeat.missed = %+v, want one plain miss", missed)
		}
	})
}

// TestOnlyTheLatestRunIsTimed: with concurrent runs, the most recently
// started one is timed; an earlier one finishing does not stop that timer.
func TestOnlyTheLatestRunIsTimed(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	const ridA, ridB = "728b3763-ea80-4113-9fc0-f49b3adf226a", "0b2f8a8e-4a46-4b0e-9a1c-3f0e2d8f1c55"
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id, fk, w := overrunHeartbeat(t, ctx, s, "parallel", "tok-parallel", base)
		ping(t, ctx, s, "tok-parallel", heartbeat.PingInput{Kind: heartbeat.PingStart, RID: ridA}, base.Add(time.Minute))
		ping(t, ctx, s, "tok-parallel", heartbeat.PingInput{Kind: heartbeat.PingStart, RID: ridB}, base.Add(2*time.Minute))
		ping(t, ctx, s, "tok-parallel", heartbeat.PingInput{Kind: heartbeat.PingSuccess, RID: ridA}, base.Add(3*time.Minute))

		checkAt(t, ctx, w, fk, base.Add(7*time.Minute+time.Second)) // B started at 2m, grace 5m
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "down" {
			t.Fatalf("run B overran: status %q, want down", hb.Status)
		}
		missed := eventsOfType(t, ctx, s, orgID, heartbeat.EventMissed)
		if len(missed) != 1 {
			t.Fatalf("heartbeat.missed events = %d, want 1", len(missed))
		}
		if d, ok := heartbeat.ParseMissDetail(missed[0].Detail); !ok || !d.StartedAt.Equal(base.Add(2*time.Minute)) {
			t.Fatalf("detail = %+v, want run B's start", d)
		}
	})
}

// TestStartArmsANewHeartbeat: a heartbeat that has never pinged and gets a
// start goes down when that run overruns, before its regular deadline.
func TestStartArmsANewHeartbeat(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatalf("BootstrapDefaultOrg: %v", err)
		}
		// CreateHeartbeat stamps created_at with the real time, so the fake
		// clock starts there.
		base := time.Now().UTC()
		id, _, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: "first", Token: "tok-first", PeriodSeconds: 86400, GraceSeconds: 60})
		if err != nil {
			t.Fatalf("CreateHeartbeat: %v", err)
		}
		ping(t, ctx, s, "tok-first", heartbeat.PingInput{Kind: heartbeat.PingStart}, base)
		fk := clock.NewFake(base)
		w := heartbeat.NewWatcher(s, fk, nil, nil)
		checkAt(t, ctx, w, fk, base.Add(61*time.Second))
		if hb := getHB(t, ctx, s, org.ID, id); hb.Status != "down" {
			t.Fatalf("new heartbeat whose first run overran: status %q, want down", hb.Status)
		}
	})
}

// TestMarkDownLosesToANewerStart: the watcher's transition is guarded on the
// run it saw, so a /start that arrives between its scan and its write (a new
// run, timed afresh) wins, and nothing is recorded.
func TestMarkDownLosesToANewerStart(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	forEachBackend(t, func(t *testing.T, s store.Store) {
		orgID, id, _, _ := overrunHeartbeat(t, ctx, s, "raced", "tok-raced-start", base)
		ping(t, ctx, s, "tok-raced-start", heartbeat.PingInput{Kind: heartbeat.PingStart}, base.Add(time.Minute))
		due, err := s.DueHeartbeats(ctx, base.Add(7*time.Minute))
		if err != nil || len(due) != 1 {
			t.Fatalf("DueHeartbeats = %v, %v; want the overrunning heartbeat", due, err)
		}
		ping(t, ctx, s, "tok-raced-start", heartbeat.PingInput{Kind: heartbeat.PingStart}, base.Add(7*time.Minute))
		fired, err := s.MarkHeartbeatDownAndEmit(ctx, due[0], heartbeat.OverranEvent(orgID, "raced", due[0].StartedAt, 300))
		if err != nil {
			t.Fatalf("MarkHeartbeatDownAndEmit: %v", err)
		}
		if fired {
			t.Fatal("fired over a newer start, want the guard to reject it")
		}
		if hb := getHB(t, ctx, s, orgID, id); hb.Status != "up" || !hb.StartedAt.Equal(base.Add(7*time.Minute)) {
			t.Fatalf("after the rejected transition: %+v", hb)
		}
		if n := len(eventsOfType(t, ctx, s, orgID, heartbeat.EventMissed)); n != 0 {
			t.Fatalf("heartbeat.missed events = %d, want 0", n)
		}
	})
}

// TestRecoveryIsStampedAfterTheMissItEnds: a ping's time is taken when the
// request arrives, before rate limiting, the body read and the wait for the
// database. If the watcher records heartbeat.missed in that gap, the
// recovery the ping then records must still be stamped after the miss:
// receivers (Ward) order a heartbeat's events by CreatedAt and refuse an
// older recovery as stale. Events are stamped when they are written.
func TestRecoveryIsStampedAfterTheMissItEnds(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, s store.Store) {
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CreateHeartbeat(ctx, heartbeat.Heartbeat{OrgID: org.ID, Name: "late", Token: "tok-late-stamp", PeriodSeconds: 60}); err != nil {
			t.Fatal(err)
		}
		real := time.Now()
		ping(t, ctx, s, "tok-late-stamp", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, real.Add(-2*time.Minute))

		arrived := time.Now() // the request arrives and takes its time here
		fk := clock.NewFake(time.Now())
		if err := heartbeat.NewWatcher(s, fk, nil, nil).Check(ctx); err != nil {
			t.Fatal(err)
		}
		missed := eventsOfType(t, ctx, s, org.ID, heartbeat.EventMissed)
		if len(missed) != 1 {
			t.Fatalf("heartbeat.missed events = %d, want 1", len(missed))
		}
		res := ping(t, ctx, s, "tok-late-stamp", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, arrived)
		if res.Event == nil || res.Event.Type != heartbeat.EventRecovered {
			t.Fatalf("ping result %+v, want a recovery", res)
		}
		rec := eventsOfType(t, ctx, s, org.ID, heartbeat.EventRecovered)
		if len(rec) != 1 || !rec[0].CreatedAt.After(missed[0].CreatedAt) || rec[0].ID <= missed[0].ID {
			t.Fatalf("recovered %v (id %d) is not after missed %v (id %d)", rec[0].CreatedAt, rec[0].ID, missed[0].CreatedAt, missed[0].ID)
		}

		// The same holds for a miss a failure ping records after a recovery.
		arrived = time.Now()
		ping(t, ctx, s, "tok-late-stamp", heartbeat.PingInput{Kind: heartbeat.PingSuccess}, time.Now())
		res = ping(t, ctx, s, "tok-late-stamp", heartbeat.PingInput{Kind: heartbeat.PingFail}, arrived)
		if res.Event == nil {
			t.Fatal("the failure recorded no event")
		}
		evs, err := s.ListHeartbeatEvents(ctx, org.ID, "late", 10)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(evs); i++ { // newest first
			if !evs[i].CreatedAt.After(evs[i+1].CreatedAt) {
				t.Fatalf("event %d (%s, %v) is not after event %d (%s, %v)", evs[i].ID, evs[i].Type, evs[i].CreatedAt, evs[i+1].ID, evs[i+1].Type, evs[i+1].CreatedAt)
			}
		}
	})
}
