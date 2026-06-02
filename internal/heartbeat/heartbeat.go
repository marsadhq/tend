// Package heartbeat implements dead-man's-switch monitoring for external jobs.
// A Heartbeat record is "up" while its owner keeps pinging; when a ping is
// missed past its period+grace deadline the Watcher marks it "down", emits a
// heartbeat.missed event, and dispatches it for notification. Recovery (down ->
// up) happens on the next ping via the store's RecordPing path.
//
// This package defines the Heartbeat domain type and depends only on core,
// clock, and the standard library. The store imports heartbeat (its
// DueHeartbeats returns []heartbeat.Heartbeat); heartbeat must NOT import store.
// The Watcher therefore talks to the store through the WatchStore consumer
// interface, which the concrete store satisfies.
package heartbeat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/core"
)

// Heartbeat is a monitored external job's dead-man's-switch record.
type Heartbeat struct {
	ID            int64
	OrgID         int64
	Name          string
	Token         string
	LastSeenAt    time.Time
	PeriodSeconds int
	GraceSeconds  int
	Status        string // "new" | "up" | "down"
	CreatedAt     time.Time
}

// NewToken returns a random 32-hex-char ping token.
func NewToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// WatchStore is the consumer interface the watcher needs.
type WatchStore interface {
	DueHeartbeats(ctx context.Context, now time.Time) ([]Heartbeat, error)
	SetHeartbeatStatus(ctx context.Context, id int64, status string) error
	// SetHeartbeatStatusIf guards the down-transition against the watcher↔ping
	// race: it updates only when both the current status and last_seen_at match
	// the values observed by DueHeartbeats. Returns false (no-op) when a
	// concurrent RecordPing changed last_seen_at before this write landed.
	SetHeartbeatStatusIf(ctx context.Context, id int64, fromStatus, toStatus string, lastSeenAt time.Time) (bool, error)
	EmitEvent(ctx context.Context, e core.Event) (int64, error)
}

// Watcher periodically scans for heartbeats that have missed their deadline.
type Watcher struct {
	store    WatchStore
	clock    clock.Clock
	dispatch func(context.Context, core.Event) // may be nil
}

// NewWatcher returns a Watcher backed by s, using c as its time source and
// dispatch to deliver heartbeat.missed events. dispatch may be nil.
func NewWatcher(s WatchStore, c clock.Clock, dispatch func(context.Context, core.Event)) *Watcher {
	return &Watcher{store: s, clock: c, dispatch: dispatch}
}

// Check marks overdue 'up' heartbeats 'down', emits heartbeat.missed, and
// dispatches it. Idempotent: a heartbeat already 'down' is not returned by
// DueHeartbeats, so it is not re-alerted.
//
// The down-transition is guarded via SetHeartbeatStatusIf: only when both the
// observed status ('up') and last_seen_at still match the stored row will the
// UPDATE fire. A RecordPing that re-stamped last_seen_at between DueHeartbeats
// and SetHeartbeatStatusIf makes the conditional UPDATE a no-op (fired==false),
// so no spurious heartbeat.missed is emitted for the now-healthy heartbeat.
//
// The status update is the commit point; the subsequent EmitEvent + dispatch
// are best-effort (errors are not retried within this Check), matching Task 6's
// recovery path. A heartbeat that was marked down but whose event was lost is
// not re-alerted on the next Check (it is no longer 'up'); this is acceptable
// for v1.
func (w *Watcher) Check(ctx context.Context) error {
	now := w.clock.Now()
	due, err := w.store.DueHeartbeats(ctx, now)
	if err != nil {
		return err
	}
	for _, hb := range due {
		fired, err := w.store.SetHeartbeatStatusIf(ctx, hb.ID, "up", "down", hb.LastSeenAt)
		if err != nil || !fired {
			continue // error, or a ping won the race → do NOT emit a spurious miss
		}
		ev := core.Event{OrgID: hb.OrgID, Type: "heartbeat.missed", Source: "heartbeat", Payload: hb.Name}
		_, _ = w.store.EmitEvent(ctx, ev)
		if w.dispatch != nil {
			w.dispatch(ctx, ev)
		}
	}
	return nil
}
