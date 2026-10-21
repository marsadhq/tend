package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
)

// The heartbeat ping log is written by the same code on both backends: the
// statements are dialect-neutral apart from their placeholders, which rebind
// rewrites, and the few dialect-specific steps (the event insert, the
// Postgres TEXT guard) go through dialectTx.

// rebind rewrites q, written with ? placeholders, for d: unchanged for SQLite,
// numbered $1, $2, ... for Postgres. The statements it is used on contain no
// literal question mark.
func rebind(d dialect, q string) string {
	if d != dialectPostgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// dialectTx is a transaction with its dialect.
type dialectTx struct {
	tx *sql.Tx
	d  dialect
}

func (t dialectTx) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, rebind(t.d, q), args...)
}

func (t dialectTx) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, rebind(t.d, q), args...)
}

// emit appends e, with the deliveries its rules match, on the transaction.
func (t dialectTx) emit(ctx context.Context, e core.Event) (int64, error) {
	if t.d == dialectPostgres {
		return pgEmitEventTx(ctx, t.tx, e)
	}
	return emitEventTx(ctx, t.tx, e)
}

// text makes s storable in a TEXT column of the backend (see pgText).
func (t dialectTx) text(s string) string {
	if t.d == dialectPostgres {
		return pgText(s)
	}
	return s
}

// recordHeartbeatPing is RecordHeartbeatPing for both backends. Everything
// it writes (the heartbeat's new state, the ping log entry, the event) is one
// transaction, so a failure writes none of it.
//
// The state change is a single UPDATE keyed on the token, so it takes the
// row (Postgres) or the database (SQLite) write lock first; concurrent pings
// for one heartbeat are serialized there. Like RecordPing before it, a
// transition is decided by compare-and-set: the first UPDATE only matches a
// row in the status the transition leaves (a success recovers a 'down' row, a
// failure takes down a row that is not), so of concurrent pings exactly one
// records it.
func recordHeartbeatPing(ctx context.Context, db *sql.DB, d dialect, token string, in heartbeat.PingInput, now time.Time) (heartbeat.PingResult, error) {
	if !heartbeat.ValidPingKind(in.Kind) {
		return heartbeat.PingResult{}, fmt.Errorf("record ping: unknown kind %q", in.Kind)
	}
	sqlTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return heartbeat.PingResult{}, fmt.Errorf("record ping begin tx: %w", err)
	}
	defer sqlTx.Rollback() //nolint:errcheck
	t := dialectTx{tx: sqlTx, d: d}

	var (
		res   heartbeat.PingResult
		event *core.Event
		seen  = formatTime(now)
	)
	scan := func(row *sql.Row) error { return row.Scan(&res.HeartbeatID, &res.OrgID, &res.Name) }
	// update runs the guarded UPDATE first and, when its guard matched no row,
	// the unguarded one; transitioned reports whether the guarded one fired.
	update := func(set, guard string, args ...any) (transitioned bool, err error) {
		args = append(args, token)
		err = scan(t.queryRow(ctx, `UPDATE heartbeats SET `+set+` WHERE token = ? AND `+guard+` RETURNING id, org_id, name`, args...))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		return false, scan(t.queryRow(ctx, `UPDATE heartbeats SET `+set+` WHERE token = ? RETURNING id, org_id, name`, args...))
	}

	switch in.Kind {
	case heartbeat.PingLog:
		// A log entry changes nothing about the heartbeat.
		err = scan(t.queryRow(ctx, `SELECT id, org_id, name FROM heartbeats WHERE token = ?`, token))
	case heartbeat.PingStart:
		// The latest start is the run being timed; it does not count as a
		// ping for the period.
		err = scan(t.queryRow(ctx,
			`UPDATE heartbeats SET started_at = ?, started_rid = ? WHERE token = ? RETURNING id, org_id, name`,
			seen, in.RID, token))
	case heartbeat.PingSuccess:
		// A success ends the run being timed when it carries that run's rid,
		// or no rid at all; a success of another run leaves it running.
		var recovered bool
		recovered, err = update(`last_seen_at = ?, status = 'up',
			started_at = CASE WHEN ? = '' OR started_rid = ? THEN NULL ELSE started_at END,
			started_rid = CASE WHEN ? = '' OR started_rid = ? THEN '' ELSE started_rid END`,
			`status = 'down'`, seen, in.RID, in.RID, in.RID, in.RID)
		if err == nil && recovered {
			ev := heartbeat.RecoveredEvent(res.OrgID, res.Name)
			event = &ev
		}
	case heartbeat.PingFail:
		// A failure takes the heartbeat down at once and ends any run.
		var wentDown bool
		wentDown, err = update(`last_seen_at = ?, status = 'down', started_at = NULL, started_rid = ''`,
			`status <> 'down'`, seen)
		if err == nil && wentDown {
			ev := heartbeat.FailedEvent(res.OrgID, res.HeartbeatID, res.Name, in.ExitStatus, in.Body)
			event = &ev
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return heartbeat.PingResult{}, ErrNotFound
	}
	if err != nil {
		return heartbeat.PingResult{}, fmt.Errorf("record ping: %w", err)
	}

	duration, err := runDuration(ctx, t, res.HeartbeatID, in, now)
	if err != nil {
		return heartbeat.PingResult{}, err
	}
	var exit sql.NullInt64
	if in.ExitStatus != nil {
		exit = sql.NullInt64{Int64: int64(*in.ExitStatus), Valid: true}
	}
	body := t.text(in.Body)
	if _, err := t.exec(ctx, `INSERT INTO heartbeat_pings
		(org_id, heartbeat_id, kind, exit_status, rid, duration_ms, body, created_at, body_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		res.OrgID, res.HeartbeatID, in.Kind, exit, in.RID, duration, nullIfEmpty(body), seen, len(body)); err != nil {
		return heartbeat.PingResult{}, fmt.Errorf("record ping log: %w", err)
	}
	// Keep the heartbeat's newest maxPingsPerHeartbeat entries, and of those
	// only as many as fit in maxPingLogBytes of body: the window of the
	// retention sweep bounds the log by age, these by size, so a client
	// pinging flat out cannot fill the disk between two sweeps. The byte sum
	// runs on the (heartbeat_id, id, body_bytes) index, never on the bodies.
	if _, err := t.exec(ctx, `DELETE FROM heartbeat_pings WHERE heartbeat_id = ? AND id <= (
		SELECT id FROM heartbeat_pings WHERE heartbeat_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		res.HeartbeatID, res.HeartbeatID, maxPingsPerHeartbeat); err != nil {
		return heartbeat.PingResult{}, fmt.Errorf("record ping: trim the ping log: %w", err)
	}
	if _, err := t.exec(ctx, `DELETE FROM heartbeat_pings WHERE heartbeat_id = ? AND id <= (
		SELECT MAX(id) FROM (
			SELECT id, SUM(body_bytes) OVER (ORDER BY id DESC) AS kept
			FROM heartbeat_pings WHERE heartbeat_id = ?
		) AS log WHERE kept > ?)`,
		res.HeartbeatID, res.HeartbeatID, maxPingLogBytes); err != nil {
		return heartbeat.PingResult{}, fmt.Errorf("record ping: hold the ping log to its byte budget: %w", err)
	}
	if event != nil {
		// The event is stamped when it is written (event.CreatedAt is zero),
		// not with the ping's arrival time: that is taken before the rate
		// limits, the body read and the wait for the database, so a
		// heartbeat.missed the watcher wrote meanwhile would be stamped later
		// than the recovery that ends it, and a receiver ordering by
		// CreatedAt would refuse the recovery as stale.
		if _, err := t.emit(ctx, *event); err != nil {
			return heartbeat.PingResult{}, err
		}
		res.Event = event
	}
	if err := sqlTx.Commit(); err != nil {
		return heartbeat.PingResult{}, fmt.Errorf("record ping commit: %w", err)
	}
	return res, nil
}

// MaxPingsPerHeartbeat is how many ping log entries a heartbeat keeps: older
// ones are dropped as new pings arrive, whatever the retention window.
const MaxPingsPerHeartbeat = 1000

// maxPingsPerHeartbeat is MaxPingsPerHeartbeat, a var so tests can lower it.
var maxPingsPerHeartbeat = MaxPingsPerHeartbeat

// MaxPingLogBytes is the most body bytes a heartbeat's ping log keeps: the
// oldest entries beyond it are dropped as new pings arrive, whatever the
// entry cap and the retention window. 1000 pings of the default 10 kB fit.
const MaxPingLogBytes = 10_000_000

// maxPingLogBytes is MaxPingLogBytes, a var so tests can lower it.
var maxPingLogBytes = MaxPingLogBytes

// runDuration is the duration, in milliseconds, of the run a success or
// failure completes, or NULL. As in Healthchecks it pairs the completion with
// the latest start that carries the same rid ("" pairs with "") within
// heartbeat.MaxRunDuration, unless a success or failure with that rid came
// after the start: each start is paired once.
func runDuration(ctx context.Context, t dialectTx, heartbeatID int64, in heartbeat.PingInput, now time.Time) (sql.NullInt64, error) {
	if in.Kind != heartbeat.PingSuccess && in.Kind != heartbeat.PingFail {
		return sql.NullInt64{}, nil
	}
	var (
		kind    string
		created sql.NullString
	)
	err := t.queryRow(ctx, `SELECT kind, created_at FROM heartbeat_pings
		WHERE heartbeat_id = ? AND rid = ? AND kind IN ('start', 'success', 'fail') AND created_at >= ?
		ORDER BY id DESC LIMIT 1`,
		heartbeatID, in.RID, formatTime(now.Add(-heartbeat.MaxRunDuration))).Scan(&kind, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, nil
	}
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("record ping: find the run's start: %w", err)
	}
	started, err := parseTime(created)
	if err != nil || kind != heartbeat.PingStart || now.Before(started) {
		return sql.NullInt64{}, nil
	}
	return sql.NullInt64{Int64: now.Sub(started).Milliseconds(), Valid: true}, nil
}

// pingColumns lists the heartbeat_pings columns in SELECT order, matched
// positionally by scanPing.
const pingColumns = `id, org_id, heartbeat_id, kind, exit_status, rid, duration_ms, body, created_at`

// scanPing reads a heartbeat.Ping from a row of pingColumns.
func scanPing(sc interface{ Scan(...any) error }) (heartbeat.Ping, error) {
	var (
		p        heartbeat.Ping
		exit     sql.NullInt64
		duration sql.NullInt64
		body     sql.NullString
		created  sql.NullString
	)
	if err := sc.Scan(&p.ID, &p.OrgID, &p.HeartbeatID, &p.Kind, &exit, &p.RID, &duration, &body, &created); err != nil {
		return heartbeat.Ping{}, err
	}
	if exit.Valid {
		n := int(exit.Int64)
		p.ExitStatus = &n
	}
	if duration.Valid {
		d := time.Duration(duration.Int64) * time.Millisecond
		p.Duration = &d
	}
	p.Body = body.String
	var err error
	if p.CreatedAt, err = parseTime(created); err != nil {
		return heartbeat.Ping{}, err
	}
	return p, nil
}

// listHeartbeatPings is ListHeartbeatPings for both backends.
func listHeartbeatPings(ctx context.Context, db *sql.DB, d dialect, orgID, heartbeatID int64, limit int) ([]heartbeat.Ping, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := db.QueryContext(ctx, rebind(d, `SELECT `+pingColumns+` FROM heartbeat_pings
		WHERE org_id = ? AND heartbeat_id = ? ORDER BY id DESC LIMIT ?`), orgID, heartbeatID, limit)
	if err != nil {
		return nil, fmt.Errorf("list heartbeat pings: %w", err)
	}
	defer rows.Close()
	var out []heartbeat.Ping
	for rows.Next() {
		p, err := scanPing(rows)
		if err != nil {
			return nil, fmt.Errorf("scan heartbeat ping: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate heartbeat pings: %w", err)
	}
	return out, nil
}

// RecordHeartbeatPing records one ping for the heartbeat that owns token; see
// Store.RecordHeartbeatPing.
func (s *SQLiteStore) RecordHeartbeatPing(ctx context.Context, token string, in heartbeat.PingInput, now time.Time) (heartbeat.PingResult, error) {
	return recordHeartbeatPing(ctx, s.db, dialectSQLite, token, in, now)
}

// ListHeartbeatPings returns a heartbeat's ping log, newest first.
func (s *SQLiteStore) ListHeartbeatPings(ctx context.Context, orgID, heartbeatID int64, limit int) ([]heartbeat.Ping, error) {
	return listHeartbeatPings(ctx, s.db, dialectSQLite, orgID, heartbeatID, limit)
}

// RecordHeartbeatPing records one ping for the heartbeat that owns token; see
// Store.RecordHeartbeatPing.
func (s *PostgresStore) RecordHeartbeatPing(ctx context.Context, token string, in heartbeat.PingInput, now time.Time) (heartbeat.PingResult, error) {
	return recordHeartbeatPing(ctx, s.db, dialectPostgres, token, in, now)
}

// ListHeartbeatPings returns a heartbeat's ping log, newest first.
func (s *PostgresStore) ListHeartbeatPings(ctx context.Context, orgID, heartbeatID int64, limit int) ([]heartbeat.Ping, error) {
	return listHeartbeatPings(ctx, s.db, dialectPostgres, orgID, heartbeatID, limit)
}

// markHeartbeatDown is MarkHeartbeatDownAndEmit for both backends. The guard
// compares last_seen_at and started_at null-safely (SQLite's IS, Postgres's IS
// NOT DISTINCT FROM): a zero time stands for NULL, a never-pinged heartbeat or
// one with no run.
func markHeartbeatDown(ctx context.Context, db *sql.DB, d dialect, observed heartbeat.Heartbeat, ev core.Event) (bool, error) {
	eq := " IS "
	if d == dialectPostgres {
		eq = " IS NOT DISTINCT FROM "
	}
	sqlTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("mark heartbeat down begin tx: %w", err)
	}
	defer sqlTx.Rollback() //nolint:errcheck
	t := dialectTx{tx: sqlTx, d: d}
	res, err := t.exec(ctx, `UPDATE heartbeats SET status = 'down', started_at = NULL, started_rid = ''
		WHERE id = ? AND status = ? AND last_seen_at`+eq+`? AND started_at`+eq+`?`,
		observed.ID, observed.Status, nullTime(observed.LastSeenAt), nullTime(observed.StartedAt))
	if err != nil {
		return false, fmt.Errorf("mark heartbeat down: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark heartbeat down rows: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if _, err := t.emit(ctx, ev); err != nil {
		return false, err
	}
	if err := sqlTx.Commit(); err != nil {
		return false, fmt.Errorf("mark heartbeat down commit: %w", err)
	}
	return true, nil
}

// MarkHeartbeatDownAndEmit moves the observed heartbeat to 'down' and records
// ev; see Store.MarkHeartbeatDownAndEmit.
func (s *SQLiteStore) MarkHeartbeatDownAndEmit(ctx context.Context, observed heartbeat.Heartbeat, ev core.Event) (bool, error) {
	return markHeartbeatDown(ctx, s.db, dialectSQLite, observed, ev)
}

// MarkHeartbeatDownAndEmit moves the observed heartbeat to 'down' and records
// ev; see Store.MarkHeartbeatDownAndEmit.
func (s *PostgresStore) MarkHeartbeatDownAndEmit(ctx context.Context, observed heartbeat.Heartbeat, ev core.Event) (bool, error) {
	return markHeartbeatDown(ctx, s.db, dialectPostgres, observed, ev)
}

// pruneHeartbeatPings is PruneHeartbeatPings for both backends.
func pruneHeartbeatPings(ctx context.Context, db *sql.DB, d dialect, before time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, rebind(d, `DELETE FROM heartbeat_pings WHERE created_at < ?`), formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("prune heartbeat pings: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneHeartbeatPings hard-deletes ping log entries created strictly before
// the cutoff and returns the rows removed.
func (s *SQLiteStore) PruneHeartbeatPings(ctx context.Context, before time.Time) (int64, error) {
	return pruneHeartbeatPings(ctx, s.db, dialectSQLite, before)
}

// PruneHeartbeatPings hard-deletes ping log entries created strictly before
// the cutoff and returns the rows removed.
func (s *PostgresStore) PruneHeartbeatPings(ctx context.Context, before time.Time) (int64, error) {
	return pruneHeartbeatPings(ctx, s.db, dialectPostgres, before)
}

// --- org ping keys (the slug form of the ping API) ---------------------------

// setOrgPingKey is SetOrgPingKey for both backends.
func setOrgPingKey(ctx context.Context, db *sql.DB, d dialect, orgID int64, key string) error {
	res, err := db.ExecContext(ctx, rebind(d, `UPDATE orgs SET ping_key = ? WHERE id = ?`), nullIfEmpty(key), orgID)
	if err != nil {
		return fmt.Errorf("set org ping key: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// getOrgPingKey is GetOrgPingKey for both backends.
func getOrgPingKey(ctx context.Context, db *sql.DB, d dialect, orgID int64) (string, error) {
	var key sql.NullString
	err := db.QueryRowContext(ctx, rebind(d, `SELECT ping_key FROM orgs WHERE id = ?`), orgID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get org ping key: %w", err)
	}
	return key.String, nil
}

// orgByPingKey is OrgByPingKey for both backends.
func orgByPingKey(ctx context.Context, db *sql.DB, d dialect, key string) (int64, error) {
	if key == "" {
		return 0, ErrNotFound
	}
	var id int64
	err := db.QueryRowContext(ctx, rebind(d, `SELECT id FROM orgs WHERE ping_key = ?`), key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("org by ping key: %w", err)
	}
	return id, nil
}

// SetOrgPingKey sets (or, with "", clears) the org's ping key.
func (s *SQLiteStore) SetOrgPingKey(ctx context.Context, orgID int64, key string) error {
	return setOrgPingKey(ctx, s.db, dialectSQLite, orgID, key)
}

// GetOrgPingKey returns the org's ping key, "" when it has none.
func (s *SQLiteStore) GetOrgPingKey(ctx context.Context, orgID int64) (string, error) {
	return getOrgPingKey(ctx, s.db, dialectSQLite, orgID)
}

// OrgByPingKey returns the org that owns key, or ErrNotFound.
func (s *SQLiteStore) OrgByPingKey(ctx context.Context, key string) (int64, error) {
	return orgByPingKey(ctx, s.db, dialectSQLite, key)
}

// SetOrgPingKey sets (or, with "", clears) the org's ping key.
func (s *PostgresStore) SetOrgPingKey(ctx context.Context, orgID int64, key string) error {
	return setOrgPingKey(ctx, s.db, dialectPostgres, orgID, key)
}

// GetOrgPingKey returns the org's ping key, "" when it has none.
func (s *PostgresStore) GetOrgPingKey(ctx context.Context, orgID int64) (string, error) {
	return getOrgPingKey(ctx, s.db, dialectPostgres, orgID)
}

// OrgByPingKey returns the org that owns key, or ErrNotFound.
func (s *PostgresStore) OrgByPingKey(ctx context.Context, key string) (int64, error) {
	return orgByPingKey(ctx, s.db, dialectPostgres, key)
}

// createHeartbeatIfAbsent is CreateHeartbeatIfAbsent for both backends.
func createHeartbeatIfAbsent(ctx context.Context, db *sql.DB, d dialect, hb heartbeat.Heartbeat) (string, bool, error) {
	var token string
	err := db.QueryRowContext(ctx, rebind(d, `INSERT INTO heartbeats
		(org_id, name, token, period_seconds, grace_seconds, status, last_seen_at, created_at)
		VALUES (?, ?, ?, ?, ?, 'new', NULL, ?)
		ON CONFLICT (org_id, name) DO NOTHING
		RETURNING token`),
		hb.OrgID, hb.Name, hb.Token, hb.PeriodSeconds, hb.GraceSeconds, nowStr()).Scan(&token)
	if err == nil {
		return token, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("create heartbeat if absent: %w", err)
	}
	var existing sql.NullString
	if err := db.QueryRowContext(ctx, rebind(d, `SELECT token FROM heartbeats WHERE org_id = ? AND name = ?`),
		hb.OrgID, hb.Name).Scan(&existing); err != nil {
		return "", false, fmt.Errorf("create heartbeat if absent: read the existing one: %w", err)
	}
	return existing.String, false, nil
}

// CreateHeartbeatIfAbsent creates hb unless its name is taken; see
// Store.CreateHeartbeatIfAbsent.
func (s *SQLiteStore) CreateHeartbeatIfAbsent(ctx context.Context, hb heartbeat.Heartbeat) (string, bool, error) {
	return createHeartbeatIfAbsent(ctx, s.db, dialectSQLite, hb)
}

// CreateHeartbeatIfAbsent creates hb unless its name is taken; see
// Store.CreateHeartbeatIfAbsent.
func (s *PostgresStore) CreateHeartbeatIfAbsent(ctx context.Context, hb heartbeat.Heartbeat) (string, bool, error) {
	return createHeartbeatIfAbsent(ctx, s.db, dialectPostgres, hb)
}
