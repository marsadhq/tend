// Package heartbeat implements dead-man's-switch monitoring for external jobs.
// A Heartbeat record is "up" while its owner keeps pinging; when a ping is
// missed past its period+grace deadline the Watcher marks it "down" and records
// a heartbeat.missed event in the same transaction, then nudges the delivery
// worker. Recovery (down -> up) happens on the next ping via the store's
// RecordPing path, which records heartbeat.recovered the same way.
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
	"io"
	"log/slog"
	"time"

	"github.com/marsadhq/kit/clock"
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

// Event types of a heartbeat's lifecycle. Every one of them carries the plain
// heartbeat name as its payload and no dedup key.
const (
	EventMissed    = "heartbeat.missed"
	EventRecovered = "heartbeat.recovered"
	EventRemoved   = "heartbeat.removed"
)

// eventSource is the Source of every heartbeat event. ListHeartbeatEvents
// finds a heartbeat's history by it and the name in the payload.
const eventSource = "heartbeat"

// MissedEvent is the event recorded when the heartbeat named name, of org
// orgID, missed its deadline.
func MissedEvent(orgID int64, name string) core.Event {
	return core.Event{OrgID: orgID, Type: EventMissed, Source: eventSource, Payload: name}
}

// RecoveredEvent is the event recorded when a ping brings a heartbeat that
// was down back up.
func RecoveredEvent(orgID int64, name string) core.Event {
	return core.Event{OrgID: orgID, Type: EventRecovered, Source: eventSource, Payload: name}
}

// RemovedEvent is the event recorded when a heartbeat is deleted.
func RemovedEvent(orgID int64, name string) core.Event {
	return core.Event{OrgID: orgID, Type: EventRemoved, Source: eventSource, Payload: name}
}

// WatchStore is the consumer interface the watcher needs.
type WatchStore interface {
	DueHeartbeats(ctx context.Context, now time.Time) ([]Heartbeat, error)
	// SetHeartbeatStatusIfAndEmit moves a heartbeat from fromStatus to
	// toStatus and records ev, with its deliveries, in one transaction. The
	// move happens only when both the current status and last_seen_at still
	// match the values observed by DueHeartbeats; it returns false, having
	// written nothing, when a concurrent RecordPing changed them first.
	SetHeartbeatStatusIfAndEmit(ctx context.Context, id int64, fromStatus, toStatus string, lastSeenAt time.Time, ev core.Event) (bool, error)
}

// Watcher periodically scans for heartbeats that have missed their deadline.
type Watcher struct {
	store    WatchStore
	clock    clock.Clock
	dispatch func(context.Context, core.Event) // may be nil
	log      *slog.Logger                      // never nil; NewWatcher supplies a default
}

// NewWatcher returns a Watcher backed by s, using c as its time source and
// dispatch to deliver heartbeat.missed events. dispatch may be nil. log may be
// nil, in which case per-heartbeat errors are discarded.
func NewWatcher(s WatchStore, c clock.Clock, dispatch func(context.Context, core.Event), log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Watcher{store: s, clock: c, dispatch: dispatch, log: log}
}

// Check marks overdue heartbeats 'down', records heartbeat.missed, and
// dispatches it. This covers both an 'up' heartbeat that stopped pinging and a 'new' one
// that never pinged (armed on created_at) — the dead-man's-switch blind spot.
// Idempotent: a heartbeat already 'down' is not returned by DueHeartbeats, so it
// is not re-alerted.
//
// The down-transition is guarded via SetHeartbeatStatusIf: only when both the
// observed status ('up' or 'new') and last_seen_at still match the stored row
// will the UPDATE fire. A RecordPing that re-stamped last_seen_at (and, for a
// 'new' heartbeat, flipped status to 'up') between DueHeartbeats and
// SetHeartbeatStatusIf makes the conditional UPDATE a no-op (fired==false), so
// no spurious heartbeat.missed is emitted for the now-healthy heartbeat.
//
// The status change and the heartbeat.missed event (with the deliveries its
// rules match) are written in ONE transaction. A heartbeat therefore never
// ends up 'down' without its alert: when the write fails, nothing is written,
// the failure is logged, and the heartbeat is still overdue on the next Check,
// which tries again. Only the dispatch afterwards is best-effort, and it only
// nudges the delivery worker, which also polls.
func (w *Watcher) Check(ctx context.Context) error {
	now := w.clock.Now()
	due, err := w.store.DueHeartbeats(ctx, now)
	if err != nil {
		return err
	}
	for _, hb := range due {
		// hb.Status is the observed 'from' status: 'up' for a heartbeat that
		// stopped pinging, or 'new' for one that never pinged at all (arming the
		// dead-man's switch on created_at). Both transition to 'down' and emit the
		// same heartbeat.missed event. The guard (status + last_seen_at, which is
		// NULL for 'new') makes a concurrent ping win the race as a no-op.
		ev := MissedEvent(hb.OrgID, hb.Name)
		fired, err := w.store.SetHeartbeatStatusIfAndEmit(ctx, hb.ID, hb.Status, "down", hb.LastSeenAt, ev)
		if err != nil {
			// Nothing was written, so the heartbeat is retried on the next
			// Check; the failure must still be visible. Do NOT abort the scan -
			// other heartbeats may still transition. A failure caused by
			// shutdown is not worth a line. Payload carries no secret
			// (heartbeat name/id only).
			if ctx.Err() == nil {
				w.log.Error("heartbeat: record missed heartbeat failed", "heartbeat", hb.Name, "id", hb.ID, "err", err)
			}
			continue
		}
		if !fired {
			continue // a ping won the race → do NOT emit a spurious miss (benign)
		}
		if w.dispatch != nil {
			w.dispatch(ctx, ev)
		}
	}
	return nil
}
