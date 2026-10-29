package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/notify"
)

// Every event, on both backends, is written by emitEvent: the event row and,
// in the same transaction, the pending deliveries its rules match. The
// statements are dialect-neutral apart from their placeholders (rebind).

// emitEventTx appends e on a SQLite transaction (see dialectTx.emitEvent).
func emitEventTx(ctx context.Context, tx *sql.Tx, e core.Event) (int64, error) {
	return dialectTx{tx: tx, d: dialectSQLite}.emitEvent(ctx, e)
}

// pgEmitEventTx appends e on a Postgres transaction (see dialectTx.emitEvent).
func pgEmitEventTx(ctx context.Context, tx *sql.Tx, e core.Event) (int64, error) {
	return dialectTx{tx: tx, d: dialectPostgres}.emitEvent(ctx, e)
}

// emitEvent appends an event on the transaction and returns its ID (see
// recordEvent).
func (t dialectTx) emitEvent(ctx context.Context, e core.Event) (int64, error) {
	rec, err := t.recordEvent(ctx, e)
	return rec.ID, err
}

// recordEvent appends an event on the transaction and returns it as it was
// recorded: with its ID, its CreatedAt (a zero one is stamped with the
// current time) and its detail. This is the single source of the
// event-insert SQL: every way an event is recorded (EmitEvent,
// FinishRunAndEmit, ReapStaleRun, a heartbeat transition, ...) goes through
// here, so it also keeps the job's failing streak for a run event
// (streakOf), recording it in the event's detail, and enqueues the event's
// deliveries, all in the same transaction.
func (t dialectTx) recordEvent(ctx context.Context, e core.Event) (core.Event, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	_, streak, err := t.streakOf(ctx, e, e.CreatedAt)
	if err != nil {
		return core.Event{}, err
	}
	e = withStreak(e, streak)
	err = t.queryRow(ctx, `INSERT INTO events
		(org_id, type, source, payload, dedup_key, created_at, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		e.OrgID, e.Type, nullIfEmpty(e.Source), nullIfEmpty(e.Payload), nullIfEmpty(e.DedupKey),
		formatTime(e.CreatedAt), nullIfEmpty(t.text(e.Detail))).Scan(&e.ID)
	if err != nil {
		return core.Event{}, fmt.Errorf("emit event: %w", err)
	}
	if err := t.enqueueDeliveries(ctx, e.ID, e); err != nil {
		return core.Event{}, err
	}
	return e, nil
}

// enqueueDeliveries inserts one pending deliveries row per channel with an
// enabled matching rule for e, on the SAME transaction that inserted the
// event. This is what makes notification delivery durable: a crash after
// commit can no longer lose a matched alert, because the pending delivery row
// commits atomically with the event. Non-alertable event types enqueue
// nothing, and only channels that exist are matched. Channels are
// deduplicated so overlapping rules (org-wide + job-scoped) yield a single
// delivery.
func (t dialectTx) enqueueDeliveries(ctx context.Context, eventID int64, e core.Event) error {
	if !notify.Alertable(e.Type) {
		return nil
	}
	rows, err := t.tx.QueryContext(ctx, rebind(t.d,
		`SELECT DISTINCT r.channel_id FROM notification_rules r
		 JOIN notification_channels c ON c.id = r.channel_id AND c.org_id = r.org_id
		 WHERE r.org_id = ? AND r.enabled = 1 AND r.event_type = ? AND (r.job_id = 0 OR r.job_id = ?)`),
		e.OrgID, e.Type, notify.EventJobID(e))
	if err != nil {
		return fmt.Errorf("enqueue deliveries match: %w", err)
	}
	var channelIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan matched channel: %w", err)
		}
		channelIDs = append(channelIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate matched channels: %w", err)
	}
	rows.Close()

	for _, chID := range channelIDs {
		if err := t.insertDelivery(ctx, e.OrgID, eventID, chID); err != nil {
			return err
		}
	}
	return nil
}

// insertDelivery queues event eventID for channel chID, due now.
func (t dialectTx) insertDelivery(ctx context.Context, orgID, eventID, chID int64) error {
	now := nowStr()
	if _, err := t.exec(ctx, `INSERT INTO deliveries
		(org_id, event_id, channel_id, attempts, state, next_attempt_at, created_at)
		VALUES (?, ?, ?, 0, ?, ?, ?)`,
		orgID, eventID, chID, notify.DeliveryPending, now, now); err != nil {
		return fmt.Errorf("enqueue delivery: %w", err)
	}
	return nil
}
