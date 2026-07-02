package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
)

// SQLiteStore is the SQLite-backed implementation of Store. It uses the
// pure-Go modernc.org/sqlite driver.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLite opens a SQLite-backed Store at the given DSN. The DSN may be a file
// path, ":memory:", or a raw modernc "file:..." connection string. WAL,
// busy_timeout, and foreign_keys pragmas are enabled so the store tolerates
// concurrent access. The returned store is not yet migrated; call Migrate.
func OpenSQLite(dsn string) (*SQLiteStore, error) {
	conn := buildDSN(dsn)
	db, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Bound the initial connectivity check so a misbehaving open never blocks
	// the daemon indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	// SQLite is a single-writer database, so serialise access at the pool: one
	// open connection avoids lock contention and also keeps a shared-cache
	// in-memory DB alive for the lifetime of the *sql.DB (it is destroyed when
	// the last connection closes).
	db.SetMaxOpenConns(1)
	return &SQLiteStore{db: db}, nil
}

// buildDSN translates a user-facing DSN into a modernc.org/sqlite connection
// string carrying the pragmas the store relies on.
func buildDSN(dsn string) string {
	// Already a fully-formed connection string: trust the caller.
	if strings.Contains(dsn, "_pragma=") {
		return dsn
	}

	const sharedPragmas = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"

	switch {
	case dsn == ":memory:" || dsn == "":
		// A shared-cache in-memory DB so all pooled connections see the same
		// data. It lives only while a connection is held open (fine for the
		// process/test lifetime). WAL is irrelevant for :memory:.
		return "file::memory:?cache=shared&" + sharedPragmas
	case strings.HasPrefix(dsn, "file:"):
		return dsn + joinQuery(dsn) + "_pragma=journal_mode(WAL)&" + sharedPragmas
	default:
		// Plain file path.
		return "file:" + dsn + "?_pragma=journal_mode(WAL)&" + sharedPragmas
	}
}

// joinQuery returns "?" or "&" depending on whether the DSN already has a query
// string, so additional pragmas can be appended.
func joinQuery(dsn string) string {
	if strings.Contains(dsn, "?") {
		return "&"
	}
	return "?"
}

// Close releases the underlying database handle.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Migrate applies all pending embedded SQLite migrations. It is idempotent.
func (s *SQLiteStore) Migrate(ctx context.Context) error {
	return runMigrations(ctx, s.db, dialectSQLite)
}

// defaultListLimit is the fallback row cap applied by list queries when the
// caller passes a non-positive limit.
const defaultListLimit = 50

// --- timestamp helpers ---------------------------------------------------

// tsLayout is the storage layout for all timestamps. It is fixed-width: always
// UTC ("Z"), always exactly 9 fractional digits. This guarantees that byte-wise
// (lexical) string comparison of two stored timestamps matches their
// chronological order, which DueJobs relies on when it compares the TEXT
// next_run column with "<= ?". time.RFC3339Nano would OMIT trailing fractional
// zeros (variable width), breaking lexical ordering at sub-second boundaries
// (e.g. "...T03:00:00Z" vs "...T03:00:00.5Z").
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders t in the fixed-width storage layout (UTC, 9 fractional
// digits, "Z" suffix).
func formatTime(t time.Time) string { return t.UTC().Format(tsLayout) }

// nullTime formats t in the fixed-width storage layout, or NULL when zero.
func nullTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(t), Valid: true}
}

// nowStr returns the current UTC time formatted for storage.
func nowStr() string { return formatTime(time.Now()) }

// parseTime maps a stored (possibly NULL) timestamp back to a time.Time; NULL or
// empty yields the zero time. It accepts the fixed-width layout as well as plain
// RFC3339(Nano) for forward-compatibility with values written by older code.
func parseTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, ns.String)
	if err != nil {
		// Fall back to plain RFC3339 for values written without sub-seconds.
		t, err = time.Parse(time.RFC3339, ns.String)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse timestamp %q: %w", ns.String, err)
		}
	}
	return t.UTC(), nil
}

// parseTimeOrZero deliberately ignores parse errors and returns the zero time
// when a stored timestamp cannot be parsed. Used for values we control on write
// (and therefore trust), where a parse failure is not actionable at the call
// site.
func parseTimeOrZero(ns sql.NullString) time.Time {
	t, _ := parseTime(ns)
	return t
}

// --- env JSON helpers ----------------------------------------------------

// encodeEnv serialises a job's env map to JSON, returning NULL for an empty map.
func encodeEnv(env map[string]string) (sql.NullString, error) {
	if len(env) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(env)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("marshal env: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// decodeEnv parses a stored env JSON string back into a map; NULL/empty yields
// nil.
func decodeEnv(ns sql.NullString) (map[string]string, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(ns.String), &m); err != nil {
		return nil, fmt.Errorf("unmarshal env: %w", err)
	}
	return m, nil
}

// --- tenancy -------------------------------------------------------------

// BootstrapDefaultOrg returns the "default" org, creating it if absent. It is
// idempotent and race-free: the INSERT ... ON CONFLICT DO NOTHING relies on the
// UNIQUE constraint on orgs(name) so concurrent callers cannot create duplicate
// default orgs, and the subsequent SELECT returns the single winning row.
func (s *SQLiteStore) BootstrapDefaultOrg(ctx context.Context) (core.Org, error) {
	const name = "default"

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO orgs (name, created_at) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`,
		name, nowStr(),
	); err != nil {
		return core.Org{}, fmt.Errorf("insert default org: %w", err)
	}

	var org core.Org
	var created sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM orgs WHERE name = ?`, name,
	).Scan(&org.ID, &org.Name, &created); err != nil {
		return core.Org{}, fmt.Errorf("query default org: %w", err)
	}
	org.CreatedAt = parseTimeOrZero(created)
	return org, nil
}

// --- jobs ----------------------------------------------------------------

// CreateJob inserts a new job and returns its ID.
func (s *SQLiteStore) CreateJob(ctx context.Context, j jobs.Job) (int64, error) {
	env, err := encodeEnv(j.Env)
	if err != nil {
		return 0, err
	}
	now := nowStr()
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs (
		org_id, name, type, command, http_url, http_method, http_body, cron,
		interval_seconds, run_at, timeout_seconds, max_retries, enabled, env,
		next_run, created_at, updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.OrgID, j.Name, string(j.Type), j.Command, j.HTTPURL, j.HTTPMethod, j.HTTPBody, j.Cron,
		j.IntervalSeconds, nullTime(j.RunAt), j.TimeoutSeconds, j.MaxRetries, boolToInt(j.Enabled), env,
		nullTime(j.NextRunAt), now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("job id: %w", err)
	}
	return id, nil
}

// jobColumns lists the jobs columns in SELECT order. THE ORDER IS LOAD-BEARING:
// scanJob reads these by ordinal position, so this list must match the column
// order of the jobs table in BOTH migrations/sqlite/0001_init.sql and
// migrations/postgres/0001_init.sql. Reorder here only if you reorder there.
const jobColumns = `id, org_id, name, type, command, http_url, http_method, http_body, cron,
	interval_seconds, run_at, timeout_seconds, max_retries, enabled, env, next_run,
	created_at, updated_at`

// scanJob reads a jobs.Job from a row produced by a SELECT of jobColumns.
func scanJob(sc interface{ Scan(...any) error }) (jobs.Job, error) {
	var (
		j        jobs.Job
		typ      string
		command  sql.NullString
		httpURL  sql.NullString
		httpMeth sql.NullString
		httpBody sql.NullString
		cron     sql.NullString
		runAt    sql.NullString
		enabled  int
		env      sql.NullString
		nextRun  sql.NullString
		created  sql.NullString
		updated  sql.NullString
	)
	if err := sc.Scan(
		&j.ID, &j.OrgID, &j.Name, &typ, &command, &httpURL, &httpMeth, &httpBody, &cron,
		&j.IntervalSeconds, &runAt, &j.TimeoutSeconds, &j.MaxRetries, &enabled, &env, &nextRun,
		&created, &updated,
	); err != nil {
		return jobs.Job{}, err
	}
	j.Type = jobs.JobType(typ)
	j.Command = command.String
	j.HTTPURL = httpURL.String
	j.HTTPMethod = httpMeth.String
	j.HTTPBody = httpBody.String
	j.Cron = cron.String
	j.Enabled = enabled != 0

	var err error
	if j.RunAt, err = parseTime(runAt); err != nil {
		return jobs.Job{}, err
	}
	if j.NextRunAt, err = parseTime(nextRun); err != nil {
		return jobs.Job{}, err
	}
	if j.CreatedAt, err = parseTime(created); err != nil {
		return jobs.Job{}, err
	}
	if j.UpdatedAt, err = parseTime(updated); err != nil {
		return jobs.Job{}, err
	}
	if j.Env, err = decodeEnv(env); err != nil {
		return jobs.Job{}, err
	}
	return j, nil
}

// GetJob returns the job with the given ID scoped to orgID, or ErrNotFound.
func (s *SQLiteStore) GetJob(ctx context.Context, orgID, id int64) (jobs.Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE org_id = ? AND id = ?`, orgID, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, fmt.Errorf("get job: %w", err)
	}
	return j, nil
}

// GetJobByName returns the job with the given name scoped to orgID, or
// ErrNotFound.
func (s *SQLiteStore) GetJobByName(ctx context.Context, orgID int64, name string) (jobs.Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE org_id = ? AND name = ?`, orgID, name)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, fmt.Errorf("get job by name: %w", err)
	}
	return j, nil
}

// ListJobs returns all jobs for an org ordered by ID.
func (s *SQLiteStore) ListJobs(ctx context.Context, orgID int64) ([]jobs.Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE org_id = ? ORDER BY id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var out []jobs.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jobs: %w", err)
	}
	return out, nil
}

// UpdateJob persists all mutable fields of a job, scoped to its OrgID and ID.
func (s *SQLiteStore) UpdateJob(ctx context.Context, j jobs.Job) error {
	env, err := encodeEnv(j.Env)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		name = ?, type = ?, command = ?, http_url = ?, http_method = ?, http_body = ?,
		cron = ?, interval_seconds = ?, run_at = ?, timeout_seconds = ?, max_retries = ?,
		enabled = ?, env = ?, next_run = ?, updated_at = ?
		WHERE org_id = ? AND id = ?`,
		j.Name, string(j.Type), j.Command, j.HTTPURL, j.HTTPMethod, j.HTTPBody,
		j.Cron, j.IntervalSeconds, nullTime(j.RunAt), j.TimeoutSeconds, j.MaxRetries,
		boolToInt(j.Enabled), env, nullTime(j.NextRunAt), nowStr(),
		j.OrgID, j.ID,
	)
	if err != nil {
		return fmt.Errorf("update job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update job rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteJob removes a job and its dependents (job_runs + job-scoped
// notification_rules) then the job row, transactionally and org-scoped. Deletes
// are explicit rather than relying on FK cascade so SQLite and Postgres behave
// identically. The job_id = id filter on notification_rules leaves all-jobs
// rules (job_id = 0) intact. Returns ErrNotFound if the job row doesn't exist
// for the org, rolling back so nothing is deleted.
func (s *SQLiteStore) DeleteJob(ctx context.Context, orgID, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete job begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM job_runs WHERE org_id = ? AND job_id = ?`, orgID, id); err != nil {
		return fmt.Errorf("delete job runs: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM notification_rules WHERE org_id = ? AND job_id = ?`, orgID, id); err != nil {
		return fmt.Errorf("delete job rules: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM jobs WHERE org_id = ? AND id = ?`, orgID, id)
	if err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete job rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// DueJobs returns enabled jobs whose next_run is set and <= now, excluding any
// job that already has a pending or running run (the no-overlap guard).
func (s *SQLiteStore) DueJobs(ctx context.Context, now time.Time) ([]jobs.Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs j
		WHERE j.enabled = 1
		  AND j.next_run IS NOT NULL
		  AND j.next_run <= ?
		  AND NOT EXISTS (
			SELECT 1 FROM job_runs r
			WHERE r.job_id = j.id AND r.status IN ('pending', 'running')
		  )
		ORDER BY j.id`,
		formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("due jobs: %w", err)
	}
	defer rows.Close()

	var out []jobs.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due job: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due jobs: %w", err)
	}
	return out, nil
}

// --- run queue + history -------------------------------------------------

// EnqueueRun inserts a pending run for a job and returns its ID.
func (s *SQLiteStore) EnqueueRun(ctx context.Context, orgID, jobID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO job_runs
		(org_id, job_id, status, attempt, created_at)
		VALUES (?, ?, ?, 1, ?)`,
		orgID, jobID, string(jobs.StatusPending), nowStr())
	if err != nil {
		return 0, fmt.Errorf("enqueue run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("run id: %w", err)
	}
	return id, nil
}

// ClaimRun atomically claims the oldest pending run, transitioning it to
// running and stamping claimed_by/started_at. ok is false when none are
// pending.
//
// The claim is a single atomic UPDATE ... RETURNING that targets the oldest
// pending row via a subquery. Doing it as one write statement (rather than a
// SELECT then UPDATE inside a transaction) avoids the classic SQLite deadlock
// where two transactions each hold a read lock and then both try to upgrade to
// a write lock. As a result concurrent workers never claim the same run and the
// store tolerates many goroutines calling ClaimRun at once.
func (s *SQLiteStore) ClaimRun(ctx context.Context, worker string) (jobs.Run, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE job_runs
		   SET status = ?, claimed_by = ?, started_at = ?
		 WHERE id = (
		   SELECT id FROM job_runs WHERE status = ? ORDER BY id LIMIT 1
		 )
		 RETURNING `+runColumns,
		string(jobs.StatusRunning), worker, nowStr(), string(jobs.StatusPending),
	)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Run{}, false, nil
	}
	if err != nil {
		return jobs.Run{}, false, fmt.Errorf("claim run: %w", err)
	}
	return run, true, nil
}

// finishRunTx records the terminal state of a run on an existing transaction.
// This is the single source of the finish-run SQL; both FinishRun and
// FinishRunAndEmit go through here.
func finishRunTx(ctx context.Context, tx *sql.Tx, runID int64, status jobs.RunStatus, exitCode int, output string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE job_runs SET status = ?, exit_code = ?, output = ?, ended_at = ? WHERE id = ?`,
		string(status), exitCode, output, nowStr(), runID)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish run rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// emitEventTx appends an event on an existing transaction and returns its ID.
// This is the single source of the event-insert SQL; both EmitEvent and
// FinishRunAndEmit go through here.
func emitEventTx(ctx context.Context, tx *sql.Tx, e core.Event) (int64, error) {
	created := e.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO events
		(org_id, type, source, payload, dedup_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		e.OrgID, e.Type, nullIfEmpty(e.Source), nullIfEmpty(e.Payload), nullIfEmpty(e.DedupKey),
		formatTime(created))
	if err != nil {
		return 0, fmt.Errorf("emit event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("event id: %w", err)
	}
	return id, nil
}

// FinishRun records the terminal state of a run.
func (s *SQLiteStore) FinishRun(ctx context.Context, runID int64, status jobs.RunStatus, exitCode int, output string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish run begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := finishRunTx(ctx, tx, runID, status, exitCode, output); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishRunAndEmit atomically records the terminal run state AND appends the
// terminal lifecycle event in a single transaction, preventing a lost terminal
// event if EmitEvent would fail after FinishRun committed.
func (s *SQLiteStore) FinishRunAndEmit(ctx context.Context, runID int64, status jobs.RunStatus, exitCode, attempt int, output string, ev core.Event) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("finish run and emit begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := finishRunTx(ctx, tx, runID, status, exitCode, output); err != nil {
		return 0, err
	}
	// Persist the final attempt count (the executor may have retried). The run
	// row was claimed with attempt=1; this records how many attempts actually ran.
	if _, err := tx.ExecContext(ctx, `UPDATE job_runs SET attempt = ? WHERE id = ?`, attempt, runID); err != nil {
		return 0, fmt.Errorf("finish run attempt: %w", err)
	}
	id, err := emitEventTx(ctx, tx, ev)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("finish run and emit commit: %w", err)
	}
	return id, nil
}

// RequeueOrphanedRuns resets every run still in 'running' back to 'pending',
// clearing claimed_by and started_at. The single-instance runner has no peer
// workers, so any 'running' row found at startup was orphaned by a crash;
// re-queueing it re-runs the work (at-least-once). Returns rows affected.
func (s *SQLiteStore) RequeueOrphanedRuns(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE job_runs SET status = ?, claimed_by = NULL, started_at = NULL WHERE status = ?`,
		string(jobs.StatusPending), string(jobs.StatusRunning))
	if err != nil {
		return 0, fmt.Errorf("requeue orphaned runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("requeue orphaned runs rows: %w", err)
	}
	return n, nil
}

// runColumns lists the job_runs columns in SELECT order. THE ORDER IS
// LOAD-BEARING: scanRun reads these by ordinal position, so this list must match
// the column order of the job_runs table in BOTH migrations/sqlite/0001_init.sql
// and migrations/postgres/0001_init.sql. Reorder here only if you reorder there.
const runColumns = `id, org_id, job_id, status, attempt, exit_code, output, claimed_by,
	started_at, ended_at, created_at`

// scanRun reads a jobs.Run from a row produced by a SELECT of runColumns.
func scanRun(sc interface{ Scan(...any) error }) (jobs.Run, error) {
	var (
		r       jobs.Run
		status  string
		output  sql.NullString
		claimed sql.NullString
		started sql.NullString
		ended   sql.NullString
		created sql.NullString
	)
	if err := sc.Scan(
		&r.ID, &r.OrgID, &r.JobID, &status, &r.Attempt, &r.ExitCode, &output, &claimed,
		&started, &ended, &created,
	); err != nil {
		return jobs.Run{}, err
	}
	r.Status = jobs.RunStatus(status)
	r.Output = output.String
	r.ClaimedBy = claimed.String

	var err error
	if r.StartedAt, err = parseTime(started); err != nil {
		return jobs.Run{}, err
	}
	if r.EndedAt, err = parseTime(ended); err != nil {
		return jobs.Run{}, err
	}
	if r.CreatedAt, err = parseTime(created); err != nil {
		return jobs.Run{}, err
	}
	return r, nil
}

// ListRuns returns up to limit runs for a job, newest first.
func (s *SQLiteStore) ListRuns(ctx context.Context, orgID, jobID int64, limit int) ([]jobs.Run, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM job_runs WHERE org_id = ? AND job_id = ? ORDER BY id DESC LIMIT ?`,
		orgID, jobID, limit)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []jobs.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	return out, nil
}

// --- secrets -------------------------------------------------------------

// PutSecret upserts a secret's ciphertext by (org, name).
func (s *SQLiteStore) PutSecret(ctx context.Context, orgID int64, name, ciphertext string) error {
	now := nowStr()
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets (org_id, name, ciphertext, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(org_id, name) DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		orgID, name, ciphertext, now, now)
	if err != nil {
		return fmt.Errorf("put secret: %w", err)
	}
	return nil
}

// GetSecret returns a secret's ciphertext, or ErrNotFound.
func (s *SQLiteStore) GetSecret(ctx context.Context, orgID int64, name string) (string, error) {
	var ct string
	err := s.db.QueryRowContext(ctx,
		`SELECT ciphertext FROM secrets WHERE org_id = ? AND name = ?`, orgID, name).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get secret: %w", err)
	}
	return ct, nil
}

// --- notification channels -----------------------------------------------

// channelColumns lists the notification_channels columns in SELECT order. THE
// ORDER IS LOAD-BEARING: scanChannel reads these by ordinal position, so this
// list must match the column order of the notification_channels table in BOTH
// migrations/sqlite/0001_init.sql and migrations/postgres/0001_init.sql. Reorder
// here only if you reorder there.
const channelColumns = `id, org_id, name, kind, config, created_at`

// scanChannel reads a notify.Channel plus its raw (encrypted) config blob from a
// row produced by a SELECT of channelColumns. The blob may be NULL, in which
// case the returned string is empty.
func scanChannel(sc interface{ Scan(...any) error }) (notify.Channel, string, error) {
	var (
		ch      notify.Channel
		kind    string
		config  sql.NullString
		created sql.NullString
	)
	if err := sc.Scan(&ch.ID, &ch.OrgID, &ch.Name, &kind, &config, &created); err != nil {
		return notify.Channel{}, "", err
	}
	ch.Kind = notify.ChannelType(kind)
	var err error
	if ch.CreatedAt, err = parseTime(created); err != nil {
		return notify.Channel{}, "", err
	}
	return ch, config.String, nil
}

// CreateChannel upserts a channel by (org, name) and returns its row ID. The
// idempotent ON CONFLICT update lets config sync re-apply a channel without
// creating duplicates; created_at is preserved on update.
func (s *SQLiteStore) CreateChannel(ctx context.Context, ch notify.Channel, configBlob string) (int64, error) {
	row := s.db.QueryRowContext(ctx, `INSERT INTO notification_channels
		(org_id, name, kind, config, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(org_id, name) DO UPDATE SET kind = excluded.kind, config = excluded.config
		RETURNING id`,
		ch.OrgID, ch.Name, string(ch.Kind), nullIfEmpty(configBlob), nowStr())
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, fmt.Errorf("create channel: %w", err)
	}
	return id, nil
}

// GetChannel returns the channel and its raw (encrypted) config blob scoped to
// orgID, or ErrNotFound.
func (s *SQLiteStore) GetChannel(ctx context.Context, orgID, id int64) (notify.Channel, string, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+channelColumns+` FROM notification_channels WHERE org_id = ? AND id = ?`, orgID, id)
	ch, blob, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return notify.Channel{}, "", ErrNotFound
	}
	if err != nil {
		return notify.Channel{}, "", fmt.Errorf("get channel: %w", err)
	}
	return ch, blob, nil
}

// ListChannels returns all channels for an org ordered by ID (no config blob).
func (s *SQLiteStore) ListChannels(ctx context.Context, orgID int64) ([]notify.Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+channelColumns+` FROM notification_channels WHERE org_id = ? ORDER BY id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	var out []notify.Channel
	for rows.Next() {
		ch, _, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channels: %w", err)
	}
	return out, nil
}

// DeleteChannel removes a channel scoped to its org.
func (s *SQLiteStore) DeleteChannel(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM notification_channels WHERE org_id = ? AND id = ?`, orgID, id)
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	return nil
}

// --- notification rules --------------------------------------------------

// ruleColumns lists the notification_rules columns in SELECT order. THE ORDER IS
// LOAD-BEARING: scanRule reads these by ordinal position, so this list must
// match the column order of the notification_rules table in BOTH
// migrations/sqlite/0001_init.sql + 0002_notifications.sql and the Postgres
// equivalents (job_id is appended LAST by 0002). Reorder here only if you
// reorder there.
const ruleColumns = `id, org_id, channel_id, event_type, enabled, created_at, job_id`

// scanRule reads a notify.Rule from a row produced by a SELECT of ruleColumns.
func scanRule(sc interface{ Scan(...any) error }) (notify.Rule, error) {
	var (
		r       notify.Rule
		enabled int
		created sql.NullString
	)
	if err := sc.Scan(&r.ID, &r.OrgID, &r.ChannelID, &r.EventType, &enabled, &created, &r.JobID); err != nil {
		return notify.Rule{}, err
	}
	r.Enabled = enabled != 0
	var err error
	if r.CreatedAt, err = parseTime(created); err != nil {
		return notify.Rule{}, err
	}
	return r, nil
}

// CreateRule upserts a rule by (org, channel, event_type, job_id) and returns
// its row ID. The idempotent ON CONFLICT update (only enabled changes) lets
// config sync re-apply a rule without creating duplicates; created_at is
// preserved on update.
func (s *SQLiteStore) CreateRule(ctx context.Context, r notify.Rule) (int64, error) {
	// RETURNING id (rather than LastInsertId) yields the canonical row id on both
	// the insert and the ON CONFLICT update paths, matching the Postgres backend
	// and avoiding SQLite's unreliable last_insert_rowid() after an upsert update.
	row := s.db.QueryRowContext(ctx, `INSERT INTO notification_rules
		(org_id, channel_id, event_type, enabled, created_at, job_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(org_id, channel_id, event_type, job_id) DO UPDATE SET enabled = excluded.enabled
		RETURNING id`,
		r.OrgID, r.ChannelID, r.EventType, boolToInt(r.Enabled), nowStr(), r.JobID)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, fmt.Errorf("create rule: %w", err)
	}
	return id, nil
}

// ListRules returns all rules for an org ordered by ID.
func (s *SQLiteStore) ListRules(ctx context.Context, orgID int64) ([]notify.Rule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM notification_rules WHERE org_id = ? ORDER BY id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()

	var out []notify.Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rules: %w", err)
	}
	return out, nil
}

// DeleteRule removes a rule scoped to its org.
func (s *SQLiteStore) DeleteRule(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM notification_rules WHERE org_id = ? AND id = ?`, orgID, id)
	if err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	return nil
}

// MatchingRules returns the enabled rules for orgID whose event_type matches and
// whose job_id is either 0 (all jobs) or equal to jobID (scoped to this event's
// job).
func (s *SQLiteStore) MatchingRules(ctx context.Context, orgID int64, eventType string, jobID int64) ([]notify.Rule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM notification_rules
		 WHERE org_id = ? AND enabled = 1 AND event_type = ? AND (job_id = 0 OR job_id = ?)
		 ORDER BY id`, orgID, eventType, jobID)
	if err != nil {
		return nil, fmt.Errorf("matching rules: %w", err)
	}
	defer rows.Close()

	var out []notify.Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan matching rule: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate matching rules: %w", err)
	}
	return out, nil
}

// --- event pipeline ------------------------------------------------------

// EmitEvent appends an event and returns its ID.
func (s *SQLiteStore) EmitEvent(ctx context.Context, e core.Event) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("emit event begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	id, err := emitEventTx(ctx, tx, e)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("emit event commit: %w", err)
	}
	return id, nil
}

// ListEvents returns up to limit events for an org, newest first.
func (s *SQLiteStore) ListEvents(ctx context.Context, orgID int64, limit int) ([]core.Event, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, type, source, payload, dedup_key, created_at
		 FROM events WHERE org_id = ? ORDER BY id DESC LIMIT ?`, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// scanEvents reads core.Events from a SELECT of the standard event column list
// (id, org_id, type, source, payload, dedup_key, created_at). It is shared by
// both backends' ListEvents and ListHeartbeatEvents (the scan is dialect-free,
// like scanHeartbeat/scanRun).
func scanEvents(rows *sql.Rows) ([]core.Event, error) {
	var out []core.Event
	for rows.Next() {
		var (
			e        core.Event
			source   sql.NullString
			payload  sql.NullString
			dedupKey sql.NullString
			created  sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.OrgID, &e.Type, &source, &payload, &dedupKey, &created); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Source = source.String
		e.Payload = payload.String
		e.DedupKey = dedupKey.String
		var err error
		if e.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return out, nil
}

// --- heartbeats ----------------------------------------------------------

// CreateHeartbeat upserts a heartbeat by (org, name), returning its row ID and
// the effective token. hb.Token is consumed ONLY on insert; on conflict the
// existing token is preserved (it is deliberately absent from the SET list) and
// RETURNING token yields whichever token now applies - so a re-sync keeps the
// existing ping URL while still refreshing period/grace. status starts 'new' on
// insert and is preserved on conflict.
func (s *SQLiteStore) CreateHeartbeat(ctx context.Context, hb heartbeat.Heartbeat) (int64, string, error) {
	row := s.db.QueryRowContext(ctx, `INSERT INTO heartbeats
		(org_id, name, token, period_seconds, grace_seconds, status, last_seen_at, created_at)
		VALUES (?, ?, ?, ?, ?, 'new', NULL, ?)
		ON CONFLICT(org_id, name) DO UPDATE SET
			period_seconds = excluded.period_seconds,
			grace_seconds = excluded.grace_seconds
		RETURNING id, token`,
		hb.OrgID, hb.Name, hb.Token, hb.PeriodSeconds, hb.GraceSeconds, nowStr())
	var (
		id    int64
		token string
	)
	if err := row.Scan(&id, &token); err != nil {
		return 0, "", fmt.Errorf("create heartbeat: %w", err)
	}
	return id, token, nil
}

// ListHeartbeats returns all heartbeats for an org ordered by ID.
func (s *SQLiteStore) ListHeartbeats(ctx context.Context, orgID int64) ([]heartbeat.Heartbeat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+heartbeatColumns+` FROM heartbeats WHERE org_id = ? ORDER BY id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list heartbeats: %w", err)
	}
	defer rows.Close()

	var out []heartbeat.Heartbeat
	for rows.Next() {
		hb, err := scanHeartbeat(rows)
		if err != nil {
			return nil, fmt.Errorf("scan heartbeat: %w", err)
		}
		out = append(out, hb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate heartbeats: %w", err)
	}
	return out, nil
}

// GetHeartbeatByName returns the heartbeat named name for orgID (token
// included), or ErrNotFound when absent.
func (s *SQLiteStore) GetHeartbeatByName(ctx context.Context, orgID int64, name string) (heartbeat.Heartbeat, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+heartbeatColumns+` FROM heartbeats WHERE org_id = ? AND name = ?`, orgID, name)
	hb, err := scanHeartbeat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return heartbeat.Heartbeat{}, ErrNotFound
	}
	if err != nil {
		return heartbeat.Heartbeat{}, fmt.Errorf("get heartbeat by name: %w", err)
	}
	return hb, nil
}

// GetHeartbeat returns the heartbeat with id for orgID, or ErrNotFound.
func (s *SQLiteStore) GetHeartbeat(ctx context.Context, orgID, id int64) (heartbeat.Heartbeat, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+heartbeatColumns+` FROM heartbeats WHERE org_id = ? AND id = ?`, orgID, id)
	hb, err := scanHeartbeat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return heartbeat.Heartbeat{}, ErrNotFound
	}
	if err != nil {
		return heartbeat.Heartbeat{}, fmt.Errorf("get heartbeat: %w", err)
	}
	return hb, nil
}

// ListHeartbeatEvents returns a heartbeat's transition events by name
// (source='heartbeat', payload=name) newest first, bounded by limit. Both
// heartbeat.missed (watcher) and heartbeat.recovered (ping handler) are emitted
// with the heartbeat name as the event payload.
func (s *SQLiteStore) ListHeartbeatEvents(ctx context.Context, orgID int64, name string, limit int) ([]core.Event, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, type, source, payload, dedup_key, created_at
		 FROM events WHERE org_id = ? AND source = 'heartbeat' AND payload = ?
		 ORDER BY id DESC LIMIT ?`, orgID, name, limit)
	if err != nil {
		return nil, fmt.Errorf("list heartbeat events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// DeleteHeartbeat removes the heartbeat with id for orgID, or ErrNotFound. Its
// past events (source='heartbeat') are intentionally left as an audit trail.
func (s *SQLiteStore) DeleteHeartbeat(ctx context.Context, orgID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM heartbeats WHERE org_id = ? AND id = ?`, orgID, id)
	if err != nil {
		return fmt.Errorf("delete heartbeat: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete heartbeat rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordPing stamps last_seen_at and sets status to 'up' for the heartbeat
// identified by token, returning whether this was a down->up recovery along with
// the heartbeat's org and name. It runs in a transaction so the status read and
// the update are consistent. ErrNotFound is returned when no heartbeat owns the
// token.
func (s *SQLiteStore) RecordPing(ctx context.Context, token string, now time.Time) (int64, string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", false, fmt.Errorf("record ping begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		orgID  int64
		name   string
		status string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT org_id, name, status FROM heartbeats WHERE token = ?`, token,
	).Scan(&orgID, &name, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, ErrNotFound
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("record ping select: %w", err)
	}
	recovered := status == "down"

	if _, err := tx.ExecContext(ctx,
		`UPDATE heartbeats SET last_seen_at = ?, status = 'up' WHERE token = ?`,
		formatTime(now), token,
	); err != nil {
		return 0, "", false, fmt.Errorf("record ping update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, "", false, fmt.Errorf("record ping commit: %w", err)
	}
	return orgID, name, recovered, nil
}

// heartbeatColumns lists the heartbeats columns in SELECT order. THE ORDER IS
// LOAD-BEARING: scanHeartbeat reads these by ordinal position, so this list must
// match the column order of the heartbeats table in BOTH
// migrations/sqlite/0001_init.sql + 0002_notifications.sql and the Postgres
// equivalents. 0001 created (id, org_id, name, last_seen_at, created_at); 0002
// APPENDED token, period_seconds, grace_seconds, status (in that order). Reorder
// here only if you reorder there.
const heartbeatColumns = `id, org_id, name, last_seen_at, created_at, token, period_seconds, grace_seconds, status`

// scanHeartbeat reads a heartbeat.Heartbeat from a row produced by a SELECT of
// heartbeatColumns. last_seen_at and token may be NULL (a 'new' heartbeat has
// neither); a NULL last_seen_at scans to the zero time.
func scanHeartbeat(sc interface{ Scan(...any) error }) (heartbeat.Heartbeat, error) {
	var (
		hb       heartbeat.Heartbeat
		lastSeen sql.NullString
		created  sql.NullString
		token    sql.NullString
	)
	if err := sc.Scan(
		&hb.ID, &hb.OrgID, &hb.Name, &lastSeen, &created, &token,
		&hb.PeriodSeconds, &hb.GraceSeconds, &hb.Status,
	); err != nil {
		return heartbeat.Heartbeat{}, err
	}
	hb.Token = token.String
	var err error
	if hb.LastSeenAt, err = parseTime(lastSeen); err != nil {
		return heartbeat.Heartbeat{}, err
	}
	if hb.CreatedAt, err = parseTime(created); err != nil {
		return heartbeat.Heartbeat{}, err
	}
	return hb, nil
}

// dueFromCandidates filters 'up' heartbeats (already SELECTed with a non-NULL
// last_seen_at) down to those strictly past their period+grace deadline at now.
// The deadline math lives in Go so it is identical across dialects. This is the
// single source of the filter; both backends call it after their SELECT.
func dueFromCandidates(candidates []heartbeat.Heartbeat, now time.Time) []heartbeat.Heartbeat {
	var out []heartbeat.Heartbeat
	for _, hb := range candidates {
		// unconfigured period => not monitored; skip to avoid immediate false misses
		// when period+grace == 0 (deadline == lastSeen, so now.After is true immediately).
		if hb.PeriodSeconds <= 0 {
			continue
		}
		deadline := hb.LastSeenAt.Add(time.Duration(hb.PeriodSeconds+hb.GraceSeconds) * time.Second)
		if now.After(deadline) {
			out = append(out, hb)
		}
	}
	return out
}

// DueHeartbeats returns the 'up' heartbeats whose period+grace deadline has
// strictly passed at now. The SELECT narrows to watched candidates ('up' with a
// last_seen_at); the deadline filter runs in Go via dueFromCandidates.
func (s *SQLiteStore) DueHeartbeats(ctx context.Context, now time.Time) ([]heartbeat.Heartbeat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+heartbeatColumns+` FROM heartbeats
		 WHERE status = 'up' AND last_seen_at IS NOT NULL
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("due heartbeats: %w", err)
	}
	defer rows.Close()

	var candidates []heartbeat.Heartbeat
	for rows.Next() {
		hb, err := scanHeartbeat(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due heartbeat: %w", err)
		}
		candidates = append(candidates, hb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due heartbeats: %w", err)
	}
	return dueFromCandidates(candidates, now), nil
}

// SetHeartbeatStatus updates a heartbeat's status by ID.
func (s *SQLiteStore) SetHeartbeatStatus(ctx context.Context, id int64, status string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE heartbeats SET status = ? WHERE id = ?`, status, id); err != nil {
		return fmt.Errorf("set heartbeat status: %w", err)
	}
	return nil
}

// SetHeartbeatStatusIf conditionally transitions a heartbeat's status from
// fromStatus to toStatus, but only when both the current status AND last_seen_at
// match the observed values. Returns (true, nil) when the row was updated, and
// (false, nil) when the guard rejected the update (a concurrent ping changed
// last_seen_at or the status no longer matches). This closes the watcher↔ping
// race: a RecordPing that re-stamped last_seen_at between DueHeartbeats and
// SetHeartbeatStatusIf makes the conditional UPDATE a no-op, so no false miss is
// emitted.
func (s *SQLiteStore) SetHeartbeatStatusIf(ctx context.Context, id int64, fromStatus, toStatus string, lastSeenAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE heartbeats SET status = ? WHERE id = ? AND status = ? AND last_seen_at = ?`,
		toStatus, id, fromStatus, formatTime(lastSeenAt))
	if err != nil {
		return false, fmt.Errorf("set heartbeat status if: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set heartbeat status if rows: %w", err)
	}
	return n > 0, nil
}

// --- run detail ----------------------------------------------------------

// GetRun returns a single run scoped to orgID, including its Output, or
// ErrNotFound for a foreign/absent id. It mirrors ListRuns' row scan
// (runColumns/scanRun), filtered by (org_id, id).
func (s *SQLiteStore) GetRun(ctx context.Context, orgID, runID int64) (jobs.Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM job_runs WHERE org_id = ? AND id = ?`, orgID, runID)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Run{}, ErrNotFound
	}
	if err != nil {
		return jobs.Run{}, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// --- secret listing ------------------------------------------------------

// ListSecrets returns the non-secret metadata (name, created_at) of every secret
// for an org, ordered by name. It SELECTs only name and created_at: the
// ciphertext column is never read, so secret material cannot leak through this
// path by construction.
func (s *SQLiteStore) ListSecrets(ctx context.Context, orgID int64) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, created_at FROM secrets WHERE org_id = ? ORDER BY name`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()

	var out []SecretMeta
	for rows.Next() {
		m, err := scanSecretMeta(rows)
		if err != nil {
			return nil, fmt.Errorf("scan secret meta: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate secrets: %w", err)
	}
	return out, nil
}

// scanSecretMeta reads a SecretMeta from a row produced by a SELECT of
// (name, created_at). It is shared by both backends (the row shape is identical).
func scanSecretMeta(sc interface{ Scan(...any) error }) (SecretMeta, error) {
	var (
		m       SecretMeta
		created sql.NullString
	)
	if err := sc.Scan(&m.Name, &created); err != nil {
		return SecretMeta{}, err
	}
	var err error
	if m.CreatedAt, err = parseTime(created); err != nil {
		return SecretMeta{}, err
	}
	return m, nil
}

// --- auth: users ---------------------------------------------------------

// userColumns lists the users columns in SELECT order. THE ORDER IS
// LOAD-BEARING: scanUser reads these by ordinal position, so this list must match
// the column order of the users table in BOTH migrations/sqlite/0001_init.sql and
// migrations/postgres/0001_init.sql. Reorder here only if you reorder there.
const userColumns = `id, org_id, email, password_hash, created_at`

// scanUser reads an auth.User from a row produced by a SELECT of userColumns.
// password_hash is nullable in the schema, so it is scanned through NullString.
func scanUser(sc interface{ Scan(...any) error }) (auth.User, error) {
	var (
		u       auth.User
		pwHash  sql.NullString
		created sql.NullString
	)
	if err := sc.Scan(&u.ID, &u.OrgID, &u.Email, &pwHash, &created); err != nil {
		return auth.User{}, err
	}
	u.PasswordHash = pwHash.String
	var err error
	if u.CreatedAt, err = parseTime(created); err != nil {
		return auth.User{}, err
	}
	return u, nil
}

// CreateUser inserts a new user and returns its ID. The UNIQUE(org_id, email)
// constraint rejects a duplicate email within an org.
func (s *SQLiteStore) CreateUser(ctx context.Context, u auth.User) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (org_id, email, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		u.OrgID, u.Email, nullIfEmpty(u.PasswordHash), nowStr())
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("user id: %w", err)
	}
	return id, nil
}

// GetUserByEmail returns the user with the given email scoped to orgID, or
// ErrNotFound.
func (s *SQLiteStore) GetUserByEmail(ctx context.Context, orgID int64, email string) (auth.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE org_id = ? AND email = ?`, orgID, email)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.User{}, ErrNotFound
	}
	if err != nil {
		return auth.User{}, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

// GetUserByID returns the user with the given ID scoped to orgID, or ErrNotFound.
func (s *SQLiteStore) GetUserByID(ctx context.Context, orgID, id int64) (auth.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE org_id = ? AND id = ?`, orgID, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.User{}, ErrNotFound
	}
	if err != nil {
		return auth.User{}, fmt.Errorf("get user by id: %w", err)
	}
	return u, nil
}

// --- auth: memberships ---------------------------------------------------

// membershipColumns lists the memberships columns in SELECT order. THE ORDER IS
// LOAD-BEARING: scanMembership reads these by ordinal position, so this list must
// match the column order of the memberships table in BOTH
// migrations/sqlite/0001_init.sql and migrations/postgres/0001_init.sql. Reorder
// here only if you reorder there.
const membershipColumns = `id, org_id, user_id, role, created_at`

// scanMembership reads an auth.Membership from a row produced by a SELECT of
// membershipColumns.
func scanMembership(sc interface{ Scan(...any) error }) (auth.Membership, error) {
	var (
		m       auth.Membership
		created sql.NullString
	)
	if err := sc.Scan(&m.ID, &m.OrgID, &m.UserID, &m.Role, &created); err != nil {
		return auth.Membership{}, err
	}
	var err error
	if m.CreatedAt, err = parseTime(created); err != nil {
		return auth.Membership{}, err
	}
	return m, nil
}

// CreateMembership inserts a new membership and returns its ID.
func (s *SQLiteStore) CreateMembership(ctx context.Context, m auth.Membership) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO memberships (org_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
		m.OrgID, m.UserID, m.Role, nowStr())
	if err != nil {
		return 0, fmt.Errorf("create membership: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("membership id: %w", err)
	}
	return id, nil
}

// GetMembership returns the membership for (orgID, userID), including the role,
// or ErrNotFound.
func (s *SQLiteStore) GetMembership(ctx context.Context, orgID, userID int64) (auth.Membership, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+membershipColumns+` FROM memberships WHERE org_id = ? AND user_id = ?`, orgID, userID)
	m, err := scanMembership(row)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Membership{}, ErrNotFound
	}
	if err != nil {
		return auth.Membership{}, fmt.Errorf("get membership: %w", err)
	}
	return m, nil
}

// --- auth: API tokens ----------------------------------------------------

// tokenListColumns lists the api_tokens columns ListTokens reads. It DELIBERATELY
// OMITS token_hash: the listing path must never carry secret material, so the
// hash is unreachable through ListTokens by construction. THE ORDER IS
// LOAD-BEARING: scanTokenMeta reads these by ordinal position.
const tokenListColumns = `id, org_id, name, created_at`

// scanTokenMeta reads an auth.APIToken (with an empty TokenHash) from a row
// produced by a SELECT of tokenListColumns. There is no token_hash column in the
// SELECT, so the returned TokenHash is always "".
func scanTokenMeta(sc interface{ Scan(...any) error }) (auth.APIToken, error) {
	var (
		t       auth.APIToken
		created sql.NullString
	)
	if err := sc.Scan(&t.ID, &t.OrgID, &t.Name, &created); err != nil {
		return auth.APIToken{}, err
	}
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return auth.APIToken{}, err
	}
	return t, nil
}

// CreateToken inserts a new API token (storing only its hash) and returns its ID.
func (s *SQLiteStore) CreateToken(ctx context.Context, t auth.APIToken) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_tokens (org_id, name, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		t.OrgID, t.Name, t.TokenHash, nowStr())
	if err != nil {
		return 0, fmt.Errorf("create token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("token id: %w", err)
	}
	return id, nil
}

// AuthenticateToken looks up a token strictly by its full hash (exact match) and
// returns the owning org ID and the token name. A miss yields ErrNotFound - no
// info leak, no partial match.
func (s *SQLiteStore) AuthenticateToken(ctx context.Context, tokenHash string) (int64, string, error) {
	var (
		orgID int64
		name  string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, name FROM api_tokens WHERE token_hash = ?`, tokenHash).Scan(&orgID, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("authenticate token: %w", err)
	}
	return orgID, name, nil
}

// ListTokens returns an org's API tokens WITHOUT the hash material: token_hash is
// not selected (see tokenListColumns), so every returned APIToken has
// TokenHash == "".
func (s *SQLiteStore) ListTokens(ctx context.Context, orgID int64) ([]auth.APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+tokenListColumns+` FROM api_tokens WHERE org_id = ? ORDER BY id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	var out []auth.APIToken
	for rows.Next() {
		t, err := scanTokenMeta(rows)
		if err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tokens: %w", err)
	}
	return out, nil
}

// DeleteToken removes a token scoped to its org; a token id belonging to another
// org is not deletable.
func (s *SQLiteStore) DeleteToken(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM api_tokens WHERE org_id = ? AND id = ?`, orgID, id)
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return nil
}

// --- small helpers -------------------------------------------------------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullIfEmpty stores empty strings as NULL to keep optional text columns clean.
func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
