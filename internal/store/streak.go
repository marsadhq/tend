package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
)

// A job's failing streak (jobs.Streak) is kept on its row: failed_runs and
// failing_since (migration 0014). It is written on both backends by the code
// below, in the transaction that records the run event, so it can never
// disagree with the events: every run.failed adds one (or starts over after
// a success), the run.recovered that ends a streak reads it, and
// FinishRunAndEmit clears it for any success.

// streakOf is the failing streak of the job a run event is about, as
// recording e leaves it: for a run.failed it is extended by e (a new streak
// starting at at when the job was not failing), for a run.recovered it is
// the streak e ends. Other events, and run events whose job is not there
// any more, have none (a zero Streak and job 0).
//
// Both read the job's row with a write lock (the UPDATE, or SELECT ... FOR
// UPDATE on Postgres), so transactions about one job's alerts take their
// turns: SQLite runs one write transaction at a time anyway.
func (t dialectTx) streakOf(ctx context.Context, e core.Event, at time.Time) (jobID int64, s jobs.Streak, err error) {
	jobID = notify.EventJobID(e)
	if jobID <= 0 {
		return 0, jobs.Streak{}, nil
	}
	var row *sql.Row
	switch e.Type {
	case jobs.EventRunFailed:
		fresh, err := t.afterSuccess(ctx, e)
		if err != nil {
			return 0, jobs.Streak{}, err
		}
		if fresh {
			row = t.queryRow(ctx, `UPDATE jobs SET failed_runs = 1, failing_since = ?
				WHERE org_id = ? AND id = ? RETURNING failed_runs, failing_since`, formatTime(at), e.OrgID, jobID)
			break
		}
		row = t.queryRow(ctx, `UPDATE jobs SET failed_runs = failed_runs + 1, failing_since = COALESCE(failing_since, ?)
			WHERE org_id = ? AND id = ? RETURNING failed_runs, failing_since`, formatTime(at), e.OrgID, jobID)
	case jobs.EventRunRecovered:
		q := `SELECT failed_runs, failing_since FROM jobs WHERE org_id = ? AND id = ?`
		if t.d == dialectPostgres {
			q += ` FOR UPDATE`
		}
		row = t.queryRow(ctx, q, e.OrgID, jobID)
	default:
		return 0, jobs.Streak{}, nil
	}
	var since sql.NullString
	err = row.Scan(&s.FailedRuns, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, jobs.Streak{}, nil // the job is gone; the event is recorded as it is
	}
	if err != nil {
		return 0, jobs.Streak{}, fmt.Errorf("job streak: %w", err)
	}
	if s.FailingSince, err = parseTime(since); err != nil {
		return 0, jobs.Streak{}, fmt.Errorf("job streak: %w", err)
	}
	if s.FailedRuns <= 0 {
		s = jobs.Streak{}
	}
	return jobID, s, nil
}

// afterSuccess reports whether the run of the run.failed e comes after a
// success: the job's previous finished run succeeded. Its failure then
// starts a new streak whatever the stored one says, so a streak that missed
// a success (one an older Tend recorded after a rollback) does not run on.
func (t dialectTx) afterSuccess(ctx context.Context, e core.Event) (bool, error) {
	var p struct {
		RunID int64 `json:"run_id"`
	}
	if json.Unmarshal([]byte(e.Payload), &p) != nil || p.RunID <= 0 {
		return false, nil
	}
	placeholder := "?"
	if t.d == dialectPostgres {
		placeholder = "$1"
	}
	prev, found, err := previousRunStatusTx(ctx, t.tx, p.RunID, placeholder)
	if err != nil {
		return false, err
	}
	return found && prev == jobs.StatusSucceeded, nil
}

// withStreak is e with its streak in its detail: a run.failed's detail gains
// it after what the runner wrote, a run.recovered's is the streak it ended.
func withStreak(e core.Event, s jobs.Streak) core.Event {
	if s.FailedRuns <= 0 {
		return e
	}
	switch e.Type {
	case jobs.EventRunFailed:
		e.Detail = jobs.WithStreak(e.Detail, s)
	case jobs.EventRunRecovered:
		if e.Detail == "" {
			e.Detail = jobs.RecoveryDetail(s)
		}
	}
	return e
}

// endStreak clears the failing streak of the job of run runID, which has just
// succeeded, and forgets the failures of it that were held (dropHeld).
func (t dialectTx) endStreak(ctx context.Context, runID int64) error {
	if _, err := t.exec(ctx, `UPDATE jobs SET failed_runs = 0, failing_since = NULL
		WHERE id = (SELECT job_id FROM job_runs WHERE id = ?) AND failed_runs <> 0`, runID); err != nil {
		return fmt.Errorf("end job streak: %w", err)
	}
	return t.dropHeld(ctx, runID)
}
