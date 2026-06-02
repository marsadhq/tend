package heartbeat_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for the raw seed/inspect handle

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/store"
)

// tsLayout mirrors the store's fixed-width storage layout so raw-seeded rows
// sort consistently with store-written rows.
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// captureDispatch records every dispatched event under a mutex so the test can
// assert on them safely.
type captureDispatch struct {
	mu     sync.Mutex
	events []core.Event
}

func (c *captureDispatch) fn(_ context.Context, e core.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureDispatch) snapshot() []core.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]core.Event, len(c.events))
	copy(out, c.events)
	return out
}

func TestWatcherDeadMansSwitch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}

	// Open our own raw handle to the same SQLite file to seed + inspect; the
	// store's RawDB is internal to package store's tests.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	t.Cleanup(func() { raw.Close() })

	// Seed one heartbeat: token "tok", period 60, grace 30, status 'new'.
	created := time.Now().UTC().Format(tsLayout)
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO heartbeats (org_id, name, token, period_seconds, grace_seconds, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		org.ID, "ext-backup", "tok", 60, 30, "new", created,
	); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	// rawStatus reads the heartbeat's current status via the raw handle.
	rawStatus := func() string {
		var st string
		if err := raw.QueryRowContext(ctx,
			`SELECT status FROM heartbeats WHERE token = ?`, "tok").Scan(&st); err != nil {
			t.Fatalf("read status: %v", err)
		}
		return st
	}

	t0 := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	fk := clock.NewFake(t0)

	// Ping at t0 -> status 'up', last_seen t0.
	if _, _, _, err := s.RecordPing(ctx, "tok", t0); err != nil {
		t.Fatalf("RecordPing t0: %v", err)
	}
	if st := rawStatus(); st != "up" {
		t.Fatalf("after ping: status = %q, want up", st)
	}

	cap := &captureDispatch{}
	w := heartbeat.NewWatcher(s, fk, cap.fn)

	// Advance 60s (total 60 <= 90 deadline): inside grace, not down.
	fk.Advance(60 * time.Second)
	if err := w.Check(ctx); err != nil {
		t.Fatalf("Check (inside grace): %v", err)
	}
	if st := rawStatus(); st != "up" {
		t.Fatalf("inside grace: status = %q, want up", st)
	}
	if n := len(cap.snapshot()); n != 0 {
		t.Fatalf("inside grace: dispatched %d events, want 0", n)
	}
	if missed := countMissed(t, ctx, s, org.ID); missed != 0 {
		t.Fatalf("inside grace: stored %d heartbeat.missed events, want 0", missed)
	}

	// Advance another 31s (total 91 > 90 deadline): now overdue.
	fk.Advance(31 * time.Second)
	if err := w.Check(ctx); err != nil {
		t.Fatalf("Check (overdue): %v", err)
	}
	if st := rawStatus(); st != "down" {
		t.Fatalf("overdue: status = %q, want down", st)
	}
	dispatched := cap.snapshot()
	if len(dispatched) != 1 {
		t.Fatalf("overdue: dispatched %d events, want 1", len(dispatched))
	}
	if dispatched[0].Type != "heartbeat.missed" {
		t.Fatalf("dispatched type = %q, want heartbeat.missed", dispatched[0].Type)
	}
	if dispatched[0].Payload != "ext-backup" {
		t.Fatalf("dispatched payload = %q, want ext-backup", dispatched[0].Payload)
	}
	stored := listMissed(t, ctx, s, org.ID)
	if len(stored) != 1 {
		t.Fatalf("overdue: stored %d heartbeat.missed events, want 1", len(stored))
	}
	if stored[0].Payload != "ext-backup" {
		t.Fatalf("stored payload = %q, want ext-backup", stored[0].Payload)
	}

	// Second Check: no re-alert (heartbeat is now 'down', excluded by DueHeartbeats).
	if err := w.Check(ctx); err != nil {
		t.Fatalf("Check (idempotent): %v", err)
	}
	if n := len(cap.snapshot()); n != 1 {
		t.Fatalf("idempotent: dispatched total %d events, want 1", n)
	}
	if missed := countMissed(t, ctx, s, org.ID); missed != 1 {
		t.Fatalf("idempotent: stored %d heartbeat.missed events, want 1", missed)
	}

	// Recovery: a ping flips it back to 'up' and reports recovered.
	_, _, recovered, err := s.RecordPing(ctx, "tok", fk.Now())
	if err != nil {
		t.Fatalf("RecordPing recovery: %v", err)
	}
	if !recovered {
		t.Fatal("recovery ping: recovered=false, want true")
	}
	if st := rawStatus(); st != "up" {
		t.Fatalf("after recovery: status = %q, want up", st)
	}
}

// listMissed returns all stored heartbeat.missed events for the org.
func listMissed(t *testing.T, ctx context.Context, s store.Store, orgID int64) []core.Event {
	t.Helper()
	evs, err := s.ListEvents(ctx, orgID, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var out []core.Event
	for _, e := range evs {
		if e.Type == "heartbeat.missed" {
			out = append(out, e)
		}
	}
	return out
}

func countMissed(t *testing.T, ctx context.Context, s store.Store, orgID int64) int {
	return len(listMissed(t, ctx, s, orgID))
}

// TestWatcherSkipsRacingPing verifies that after a fresh RecordPing that
// re-stamps last_seen_at to a non-overdue time, Check leaves the heartbeat 'up'
// and emits no heartbeat.missed event. The guard in SetHeartbeatStatusIf rejects
// the stale last_seen_at the watcher would have observed from DueHeartbeats,
// preventing a spurious miss. (The pure watcher↔ping interleave race is covered
// by the store-level TestSetHeartbeatStatusIf; this test validates the watcher's
// end-to-end behaviour when no heartbeat is overdue after a ping.)
func TestWatcherSkipsRacingPing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "race.db")

	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	org, err := s.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	t.Cleanup(func() { raw.Close() })

	t0 := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	// Seed the heartbeat as 'up', last seen at t0.
	created := t0.UTC().Format(tsLayout)
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO heartbeats (org_id, name, token, period_seconds, grace_seconds, status, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		org.ID, "guarded-hb", "tok-guard", 60, 30, "up", t0.UTC().Format(tsLayout), created,
	); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	// Advance clock to t0+91s so the heartbeat is overdue at that instant.
	// Then simulate "ping won the race": call RecordPing with the current (non-overdue) time,
	// re-stamping last_seen_at to t0+91s (deadline is last_seen+90s = t0+181s, not overdue).
	overdue := t0.Add(91 * time.Second)
	fk := clock.NewFake(overdue)

	// RecordPing at overdue time - this re-stamps last_seen_at to t0+91s.
	// The new deadline would be (t0+91s)+90s = t0+181s, which is after fk.Now()=t0+91s.
	if _, _, _, err := s.RecordPing(ctx, "tok-guard", overdue); err != nil {
		t.Fatalf("RecordPing: %v", err)
	}

	cap := &captureDispatch{}
	w := heartbeat.NewWatcher(s, fk, cap.fn)

	// Check at t0+91s: last_seen is now t0+91s, deadline = t0+181s → not overdue.
	// DueHeartbeats should return nothing. No miss, no dispatch.
	if err := w.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}

	// Status must still be 'up'.
	var status string
	if err := raw.QueryRowContext(ctx,
		`SELECT status FROM heartbeats WHERE token = ?`, "tok-guard").Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "up" {
		t.Fatalf("status = %q after ping+Check, want up", status)
	}

	if n := len(cap.snapshot()); n != 0 {
		t.Fatalf("dispatched %d events after fresh ping, want 0", n)
	}

	evs, err := s.ListEvents(ctx, org.ID, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, e := range evs {
		if e.Type == "heartbeat.missed" {
			t.Fatalf("unexpected heartbeat.missed event emitted after fresh ping: %+v", e)
		}
	}
}

func TestNewToken(t *testing.T) {
	a, err := heartbeat.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if len(a) != 32 {
		t.Fatalf("token length = %d, want 32", len(a))
	}
	for _, r := range a {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("token %q contains non-hex char %q", a, r)
		}
	}
	b, err := heartbeat.NewToken()
	if err != nil {
		t.Fatalf("NewToken (2): %v", err)
	}
	if a == b {
		t.Fatalf("two NewToken calls returned identical tokens %q", a)
	}
}
