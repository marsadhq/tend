package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	kitnotify "github.com/marsadhq/kit/notify"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
)

// The pacing of failure alerts (notify.Pacing) is applied here, on both
// backends, in the transaction that records each run event: a run.failed is
// queued for a channel when notify.PaceFailure says so, a run.recovered when
// notify.PaceRecovery does, and what each channel was told about each job is
// kept in failure_alerts (migration 0017). On Postgres every transaction
// about one job takes its rows in one order: its runs, then the job's row
// (streakOf, releaseOne, DeleteJob), then its failure_alerts rows, so they
// take their turns instead of deadlocking.

// matchedChannel is a channel with an enabled rule matching an event: its
// kind and the most frequent reminder interval of its matching rules
// (notify.EffectiveRepeat), since each of them would send what it sends.
type matchedChannel struct {
	id     int64
	kind   kitnotify.ChannelType
	repeat time.Duration
}

// matchedChannels returns the channels that exist and have an enabled rule
// for e (org-wide, or scoped to its job), each once, by id.
func (t dialectTx) matchedChannels(ctx context.Context, e core.Event, jobID int64) ([]matchedChannel, error) {
	rows, err := t.tx.QueryContext(ctx, rebind(t.d,
		`SELECT r.channel_id, c.kind, r.repeat_seconds FROM notification_rules r
		 JOIN notification_channels c ON c.id = r.channel_id AND c.org_id = r.org_id
		 WHERE r.org_id = ? AND r.enabled = 1 AND r.event_type = ? AND (r.job_id = 0 OR r.job_id = ?)
		 ORDER BY r.channel_id, r.id`),
		e.OrgID, e.Type, jobID)
	if err != nil {
		return nil, fmt.Errorf("enqueue deliveries match: %w", err)
	}
	defer rows.Close()
	var out []matchedChannel
	for rows.Next() {
		var (
			id     int64
			kind   string
			repeat sql.NullInt64
		)
		if err := rows.Scan(&id, &kind, &repeat); err != nil {
			return nil, fmt.Errorf("scan matched channel: %w", err)
		}
		r := notify.EffectiveRepeat(kitnotify.ChannelType(kind), repeatOf(repeat))
		if n := len(out); n > 0 && out[n-1].id == id {
			out[n-1].repeat = min(out[n-1].repeat, r)
			continue
		}
		out = append(out, matchedChannel{id: id, kind: kitnotify.ChannelType(kind), repeat: r})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate matched channels: %w", err)
	}
	return out, nil
}

// repeatOf is a stored repeat_seconds as a rule's Repeat (nil when unset).
func repeatOf(n sql.NullInt64) *time.Duration {
	if !n.Valid {
		return nil
	}
	d := time.Duration(n.Int64) * time.Second
	return &d
}

// paced reports whether e goes to channel ch, applying the pacing to the
// run events of a job (jobID) with a known streak, and keeps what the
// channel was told. Every other event goes to every matched channel.
func (t dialectTx) paced(ctx context.Context, e core.Event, jobID int64, streak jobs.Streak, ch matchedChannel) (bool, error) {
	if jobID <= 0 {
		return true, nil
	}
	switch e.Type {
	case jobs.EventRunFailed:
		if streak.FailedRuns <= 0 {
			return true, nil
		}
		st, known, _, err := t.alertState(ctx, ch.id, jobID)
		if err != nil {
			return false, err
		}
		send, next := notify.PaceFailure(st, known, e.CreatedAt, streak.FailingSince, ch.repeat)
		var held sql.NullInt64
		if next.Held {
			held = sql.NullInt64{Int64: e.ID, Valid: true}
		}
		return send, t.saveAlertState(ctx, e.OrgID, ch.id, jobID, next, held)
	case jobs.EventRunRecovered:
		// A channel that does not get the job's failures (any more) gets
		// its recoveries, whatever it was told before.
		_, routed, err := t.failedRepeat(ctx, e.OrgID, ch.id, jobID, ch.kind)
		if err != nil {
			return false, err
		}
		if !routed {
			return true, nil
		}
		st, known, _, err := t.alertState(ctx, ch.id, jobID)
		if err != nil {
			return false, err
		}
		return notify.PaceRecovery(st, known, streak.FailingSince), nil
	}
	return true, nil
}

// alertState reads what channel chID was told about job jobID's failures;
// known is false when nothing is kept yet.
func (t dialectTx) alertState(ctx context.Context, chID, jobID int64) (st notify.AlertState, known bool, held int64, err error) {
	var last, anchor sql.NullString
	var heldID sql.NullInt64
	err = t.queryRow(ctx, `SELECT last_sent_at, anchor_at, held_event_id FROM failure_alerts
		WHERE channel_id = ? AND job_id = ?`, chID, jobID).Scan(&last, &anchor, &heldID)
	if errors.Is(err, sql.ErrNoRows) {
		return notify.AlertState{}, false, 0, nil
	}
	if err != nil {
		return notify.AlertState{}, false, 0, fmt.Errorf("read failure alert state: %w", err)
	}
	if st.LastSentAt, err = parseTime(last); err != nil {
		return notify.AlertState{}, false, 0, fmt.Errorf("read failure alert state: %w", err)
	}
	if st.Anchor, err = parseTime(anchor); err != nil {
		return notify.AlertState{}, false, 0, fmt.Errorf("read failure alert state: %w", err)
	}
	st.Held = heldID.Valid
	return st, true, heldID.Int64, nil
}

// saveAlertState keeps st for channel chID and job jobID, with held the
// held run.failed's event id (NULL: none).
func (t dialectTx) saveAlertState(ctx context.Context, orgID, chID, jobID int64, st notify.AlertState, held sql.NullInt64) error {
	if _, err := t.exec(ctx, `INSERT INTO failure_alerts
		(org_id, channel_id, job_id, last_sent_at, anchor_at, held_event_id) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (channel_id, job_id) DO UPDATE SET
			last_sent_at = excluded.last_sent_at, anchor_at = excluded.anchor_at, held_event_id = excluded.held_event_id`,
		orgID, chID, jobID, nullTime(st.LastSentAt), nullTime(st.Anchor), held); err != nil {
		return fmt.Errorf("keep failure alert state: %w", err)
	}
	return nil
}

// dropHeld forgets the held failures of the job of run runID, which has
// just succeeded: the channels that were not told of its streak are not
// told of it now.
func (t dialectTx) dropHeld(ctx context.Context, runID int64) error {
	if _, err := t.exec(ctx, `UPDATE failure_alerts SET held_event_id = NULL
		WHERE job_id = (SELECT job_id FROM job_runs WHERE id = ?) AND held_event_id IS NOT NULL`, runID); err != nil {
		return fmt.Errorf("drop held failure alerts: %w", err)
	}
	return nil
}

// deleteAlertStates deletes what is kept about the alerts of a job or of a
// channel (column "job_id" or "channel_id"), which is being deleted.
func (t dialectTx) deleteAlertStates(ctx context.Context, orgID int64, column string, id int64) error {
	if _, err := t.exec(ctx, `DELETE FROM failure_alerts WHERE org_id = ? AND `+column+` = ?`, orgID, id); err != nil {
		return fmt.Errorf("delete failure alert state: %w", err)
	}
	return nil
}

// heldCandidate is a held failure.
type heldCandidate struct {
	orgID, channelID, jobID, eventID int64
	kind                             kitnotify.ChannelType
}

// releaseHeld queues, for both backends, the held failures whose hold has
// ended at now (notify.ReleaseDue, under the channel's rules as they are
// now): the job is still failing and the channel still routes its
// run.failed. Each is its own transaction, which takes the job's row lock
// before it writes the job's alert state. A held failure whose job recovered
// (by its streak or by its last run) or went, whose channel no longer routes
// it, or whose event is gone is dropped. It returns how many were queued. Held failures are few (a
// flapping job's, one per channel), so every one is looked at each time.
func releaseHeld(ctx context.Context, db *sql.DB, d dialect, now time.Time) (int, error) {
	// Candidates are read first, and the rows closed, because SQLite has one
	// connection and each release needs it for its own transaction.
	rows, err := db.QueryContext(ctx, `SELECT fa.org_id, fa.channel_id, fa.job_id, fa.held_event_id, c.kind
		FROM failure_alerts fa JOIN notification_channels c ON c.id = fa.channel_id AND c.org_id = fa.org_id
		WHERE fa.held_event_id IS NOT NULL
		ORDER BY fa.last_sent_at, fa.channel_id, fa.job_id`)
	if err != nil {
		return 0, fmt.Errorf("release held alerts: %w", err)
	}
	var cands []heldCandidate
	for rows.Next() {
		var c heldCandidate
		var kind string
		if err := rows.Scan(&c.orgID, &c.channelID, &c.jobID, &c.eventID, &kind); err != nil {
			rows.Close()
			return 0, fmt.Errorf("release held alerts: scan: %w", err)
		}
		c.kind = kitnotify.ChannelType(kind)
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("release held alerts: %w", err)
	}
	rows.Close()

	released := 0
	for _, c := range cands {
		ok, err := releaseOne(ctx, db, d, c, now)
		if err != nil {
			return released, err
		}
		if ok {
			released++
		}
	}
	return released, nil
}

// releaseOne releases one held failure, if it is still held and due. Whether
// it is due is read first, without a lock, since it mostly is not; only then
// is the job's row locked and the state read again.
func releaseOne(ctx context.Context, db *sql.DB, d dialect, c heldCandidate, now time.Time) (bool, error) {
	sqlTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("release held alert begin tx: %w", err)
	}
	defer sqlTx.Rollback() //nolint:errcheck
	t := dialectTx{tx: sqlTx, d: d}

	// drop forgets the held failure (it is not sent) unless it changed meanwhile.
	drop := func() (bool, error) {
		if _, err := t.exec(ctx, `UPDATE failure_alerts SET held_event_id = NULL
			WHERE channel_id = ? AND job_id = ? AND held_event_id = ?`, c.channelID, c.jobID, c.eventID); err != nil {
			return false, fmt.Errorf("drop held alert: %w", err)
		}
		return false, sqlTx.Commit()
	}
	// stillHeld reads the channel's state for the job and whether c is still
	// what it holds.
	stillHeld := func() (notify.AlertState, bool, error) {
		st, known, held, err := t.alertState(ctx, c.channelID, c.jobID)
		return st, known && held == c.eventID, err
	}

	st, held, err := stillHeld()
	if err != nil || !held {
		return false, err // sent, superseded or dropped meanwhile
	}
	repeat, routed, err := t.failedRepeat(ctx, c.orgID, c.channelID, c.jobID, c.kind)
	if err != nil {
		return false, err
	}
	if !routed {
		return drop()
	}
	if !notify.ReleaseDue(st, now, repeat) {
		return false, nil
	}

	lock := `SELECT failed_runs FROM jobs WHERE org_id = ? AND id = ?`
	if d == dialectPostgres {
		lock += ` FOR UPDATE`
	}
	var failedRuns int
	err = t.queryRow(ctx, lock, c.orgID, c.jobID).Scan(&failedRuns)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && failedRuns <= 0) {
		return drop()
	}
	if err != nil {
		return false, fmt.Errorf("release held alert: lock job: %w", err)
	}
	pause(pauseReleaseJobLocked)
	// The job's runs have the last word: a success the streak did not see
	// (one an older Tend recorded after a rollback) means it is not failing.
	var last string
	err = t.queryRow(ctx, `SELECT status FROM job_runs
		WHERE org_id = ? AND job_id = ? AND status NOT IN ('pending', 'running') AND ended_at IS NOT NULL
		ORDER BY ended_at DESC, id DESC LIMIT 1`, c.orgID, c.jobID).Scan(&last)
	if err == nil && jobs.RunStatus(last) == jobs.StatusSucceeded {
		return drop()
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("release held alert: last run: %w", err)
	}
	if st, held, err = stillHeld(); err != nil || !held || !notify.ReleaseDue(st, now, repeat) {
		return false, err
	}
	var one int
	err = t.queryRow(ctx, `SELECT 1 FROM events WHERE id = ?`, c.eventID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return drop()
	}
	if err != nil {
		return false, fmt.Errorf("release held alert: event: %w", err)
	}
	if err := t.saveAlertState(ctx, c.orgID, c.channelID, c.jobID, notify.Released(now), sql.NullInt64{}); err != nil {
		return false, err
	}
	if err := t.insertDelivery(ctx, c.orgID, c.eventID, c.channelID); err != nil {
		return false, err
	}
	if err := sqlTx.Commit(); err != nil {
		return false, fmt.Errorf("release held alert commit: %w", err)
	}
	return true, nil
}

// failedRepeat is the reminder interval for job jobID's failures on channel
// chID (the most frequent of its enabled matching run.failed rules), and
// whether any rule routes them there at all.
func (t dialectTx) failedRepeat(ctx context.Context, orgID, chID, jobID int64, kind kitnotify.ChannelType) (time.Duration, bool, error) {
	rows, err := t.tx.QueryContext(ctx, rebind(t.d, `SELECT repeat_seconds FROM notification_rules
		WHERE org_id = ? AND channel_id = ? AND enabled = 1 AND event_type = ? AND (job_id = 0 OR job_id = ?)`),
		orgID, chID, jobs.EventRunFailed, jobID)
	if err != nil {
		return 0, false, fmt.Errorf("run.failed rules: %w", err)
	}
	defer rows.Close()
	var (
		repeat time.Duration
		routed bool
	)
	for rows.Next() {
		var n sql.NullInt64
		if err := rows.Scan(&n); err != nil {
			return 0, false, fmt.Errorf("run.failed rules: %w", err)
		}
		r := notify.EffectiveRepeat(kind, repeatOf(n))
		if !routed || r < repeat {
			repeat = r
		}
		routed = true
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("run.failed rules: %w", err)
	}
	return repeat, routed, nil
}

// ReleaseHeldAlerts queues the held failure alerts whose hold has ended at
// now (see releaseHeld). The delivery worker calls it before each claim.
func (s *SQLiteStore) ReleaseHeldAlerts(ctx context.Context, now time.Time) (int, error) {
	return releaseHeld(ctx, s.db, dialectSQLite, now)
}

// ReleaseHeldAlerts queues the held failure alerts whose hold has ended at
// now (see releaseHeld). The delivery worker calls it before each claim.
func (s *PostgresStore) ReleaseHeldAlerts(ctx context.Context, now time.Time) (int, error) {
	return releaseHeld(ctx, s.db, dialectPostgres, now)
}
