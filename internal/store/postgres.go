package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
)

// PostgresStore is the Postgres-backed implementation of Store. It uses the pgx
// stdlib driver via database/sql. Unlike SQLite it allows real connection-level
// concurrency, which is what makes the FOR UPDATE SKIP LOCKED claim meaningful.
//
// Data is stored in the same shapes as SQLite (timestamps as RFC3339 TEXT,
// flags/counters as INTEGER, env as JSON TEXT) so the row-scan and value
// helpers in sqlite.go are reused unchanged. The methods below differ only in
// using $N placeholders and RETURNING id (the pgx stdlib driver does not
// implement Result.LastInsertId).
type PostgresStore struct {
	db *sql.DB
}

// Compile-time assertion that *PostgresStore implements Store.
var _ Store = (*PostgresStore)(nil)

// OpenPostgres opens a Postgres-backed Store at the given DSN (a
// "postgres://..." connection string). The returned store is not yet migrated;
// call Migrate.
func OpenPostgres(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	// Bound the initial connectivity check so a network connect never blocks the
	// daemon indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	// Allow genuine concurrency so concurrent ClaimRun callers contend at the
	// database (where SKIP LOCKED resolves the race), not at the pool.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	// Recycle connections in a long-lived daemon to avoid using stale ones (e.g.
	// after a server-side timeout or a transparent failover).
	db.SetConnMaxLifetime(time.Hour)
	return &PostgresStore{db: db}, nil
}

// Close releases the underlying database handle.
func (s *PostgresStore) Close() error { return s.db.Close() }

// Migrate applies all pending embedded Postgres migrations. It is idempotent.
func (s *PostgresStore) Migrate(ctx context.Context) error {
	return runMigrations(ctx, s.db, dialectPostgres)
}

// --- tenancy -------------------------------------------------------------

// BootstrapDefaultOrg returns the "default" org, creating it if absent. It is
// idempotent and race-free via the UNIQUE constraint on orgs(name).
func (s *PostgresStore) BootstrapDefaultOrg(ctx context.Context) (core.Org, error) {
	const name = "default"

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO orgs (name, created_at) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`,
		name, nowStr(),
	); err != nil {
		return core.Org{}, fmt.Errorf("insert default org: %w", err)
	}

	var org core.Org
	var created sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM orgs WHERE name = $1`, name,
	).Scan(&org.ID, &org.Name, &created); err != nil {
		return core.Org{}, fmt.Errorf("query default org: %w", err)
	}
	org.CreatedAt = parseTimeOrZero(created)
	return org, nil
}

// --- jobs ----------------------------------------------------------------

// CreateJob inserts a new job and returns its ID (via RETURNING id).
func (s *PostgresStore) CreateJob(ctx context.Context, j jobs.Job) (int64, error) {
	env, err := encodeEnv(j.Env)
	if err != nil {
		return 0, err
	}
	now := nowStr()
	var id int64
	err = s.db.QueryRowContext(ctx, `INSERT INTO jobs (
		org_id, name, type, command, http_url, http_method, http_body, cron,
		interval_seconds, run_at, timeout_seconds, max_retries, enabled, env,
		next_run, created_at, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	RETURNING id`,
		j.OrgID, j.Name, string(j.Type), j.Command, j.HTTPURL, j.HTTPMethod, j.HTTPBody, j.Cron,
		j.IntervalSeconds, nullTime(j.RunAt), j.TimeoutSeconds, j.MaxRetries, boolToInt(j.Enabled), env,
		nullTime(j.NextRunAt), now, now,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert job: %w", err)
	}
	return id, nil
}

// GetJob returns the job with the given ID scoped to orgID, or ErrNotFound.
func (s *PostgresStore) GetJob(ctx context.Context, orgID, id int64) (jobs.Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE org_id = $1 AND id = $2`, orgID, id)
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
func (s *PostgresStore) GetJobByName(ctx context.Context, orgID int64, name string) (jobs.Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE org_id = $1 AND name = $2`, orgID, name)
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
func (s *PostgresStore) ListJobs(ctx context.Context, orgID int64) ([]jobs.Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE org_id = $1 ORDER BY id`, orgID)
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
func (s *PostgresStore) UpdateJob(ctx context.Context, j jobs.Job) error {
	env, err := encodeEnv(j.Env)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		name = $1, type = $2, command = $3, http_url = $4, http_method = $5, http_body = $6,
		cron = $7, interval_seconds = $8, run_at = $9, timeout_seconds = $10, max_retries = $11,
		enabled = $12, env = $13, next_run = $14, updated_at = $15
		WHERE org_id = $16 AND id = $17`,
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
func (s *PostgresStore) DeleteJob(ctx context.Context, orgID, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete job begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM job_runs WHERE org_id = $1 AND job_id = $2`, orgID, id); err != nil {
		return fmt.Errorf("delete job runs: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM notification_rules WHERE org_id = $1 AND job_id = $2`, orgID, id); err != nil {
		return fmt.Errorf("delete job rules: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM jobs WHERE org_id = $1 AND id = $2`, orgID, id)
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
func (s *PostgresStore) DueJobs(ctx context.Context, now time.Time) ([]jobs.Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs j
		WHERE j.enabled = 1
		  AND j.next_run IS NOT NULL
		  AND j.next_run <= $1
		  AND NOT EXISTS (
			SELECT 1 FROM job_runs r
			WHERE r.job_id = j.id AND r.status IN ('pending', 'running')
		  )
		ORDER BY j.id`,
		now.UTC().Format(tsLayout))
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

// EnqueueRun inserts a pending run for a job and returns its ID (RETURNING id).
func (s *PostgresStore) EnqueueRun(ctx context.Context, orgID, jobID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO job_runs
		(org_id, job_id, status, attempt, created_at)
		VALUES ($1, $2, $3, 1, $4)
		RETURNING id`,
		orgID, jobID, string(jobs.StatusPending), nowStr()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("enqueue run: %w", err)
	}
	return id, nil
}

// ClaimRun atomically claims the oldest pending run, transitioning it to
// running and stamping claimed_by/started_at. ok is false when none are
// pending.
//
// Exclusivity comes from FOR UPDATE SKIP LOCKED in the subquery: concurrent
// workers each lock and consume a distinct pending row (a worker skips rows
// another worker has already locked), so two workers never claim the same run.
// The whole thing is a single UPDATE ... RETURNING; no pending row means
// sql.ErrNoRows, which maps to ok=false.
func (s *PostgresStore) ClaimRun(ctx context.Context, worker string) (jobs.Run, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE job_runs
		   SET status = $1, claimed_by = $2, started_at = $3
		 WHERE id = (
		   SELECT id FROM job_runs
		   WHERE status = $4
		   ORDER BY id
		   FOR UPDATE SKIP LOCKED
		   LIMIT 1
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

// pgFinishRunTx records the terminal state of a run on an existing transaction.
// This is the single source of the Postgres finish-run SQL; both FinishRun and
// FinishRunAndEmit go through here.
func pgFinishRunTx(ctx context.Context, tx *sql.Tx, runID int64, status jobs.RunStatus, exitCode int, output string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE job_runs SET status = $1, exit_code = $2, output = $3, ended_at = $4 WHERE id = $5`,
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

// pgEmitEventTx appends an event on an existing Postgres transaction and
// returns its ID via RETURNING id. This is the single source of the Postgres
// event-insert SQL; both EmitEvent and FinishRunAndEmit go through here.
func pgEmitEventTx(ctx context.Context, tx *sql.Tx, e core.Event) (int64, error) {
	created := e.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO events
		(org_id, type, source, payload, dedup_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		e.OrgID, e.Type, nullIfEmpty(e.Source), nullIfEmpty(e.Payload), nullIfEmpty(e.DedupKey),
		created.UTC().Format(tsLayout)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("emit event: %w", err)
	}
	return id, nil
}

// FinishRun records the terminal state of a run.
func (s *PostgresStore) FinishRun(ctx context.Context, runID int64, status jobs.RunStatus, exitCode int, output string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish run begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := pgFinishRunTx(ctx, tx, runID, status, exitCode, output); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishRunAndEmit atomically records the terminal run state AND appends the
// terminal lifecycle event in a single transaction, preventing a lost terminal
// event if EmitEvent would fail after FinishRun committed.
func (s *PostgresStore) FinishRunAndEmit(ctx context.Context, runID int64, status jobs.RunStatus, exitCode, attempt int, output string, ev core.Event) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("finish run and emit begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := pgFinishRunTx(ctx, tx, runID, status, exitCode, output); err != nil {
		return 0, err
	}
	// Persist the final attempt count (the executor may have retried). The run
	// row was claimed with attempt=1; this records how many attempts actually ran.
	if _, err := tx.ExecContext(ctx, `UPDATE job_runs SET attempt = $1 WHERE id = $2`, attempt, runID); err != nil {
		return 0, fmt.Errorf("finish run attempt: %w", err)
	}
	id, err := pgEmitEventTx(ctx, tx, ev)
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
func (s *PostgresStore) RequeueOrphanedRuns(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE job_runs SET status = $1, claimed_by = NULL, started_at = NULL WHERE status = $2`,
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

// ListRuns returns up to limit runs for a job, newest first.
func (s *PostgresStore) ListRuns(ctx context.Context, orgID, jobID int64, limit int) ([]jobs.Run, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM job_runs WHERE org_id = $1 AND job_id = $2 ORDER BY id DESC LIMIT $3`,
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
func (s *PostgresStore) PutSecret(ctx context.Context, orgID int64, name, ciphertext string) error {
	now := nowStr()
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets (org_id, name, ciphertext, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, name) DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		orgID, name, ciphertext, now, now)
	if err != nil {
		return fmt.Errorf("put secret: %w", err)
	}
	return nil
}

// GetSecret returns a secret's ciphertext, or ErrNotFound.
func (s *PostgresStore) GetSecret(ctx context.Context, orgID int64, name string) (string, error) {
	var ct string
	err := s.db.QueryRowContext(ctx,
		`SELECT ciphertext FROM secrets WHERE org_id = $1 AND name = $2`, orgID, name).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get secret: %w", err)
	}
	return ct, nil
}

// --- notification channels -----------------------------------------------

// CreateChannel upserts a channel by (org, name) and returns its row ID via
// RETURNING id. The idempotent ON CONFLICT update lets config sync re-apply a
// channel without creating duplicates; created_at is preserved on update.
//
// scanChannel and channelColumns are shared with the SQLite backend (defined in
// sqlite.go), since both store the same row shapes.
func (s *PostgresStore) CreateChannel(ctx context.Context, ch notify.Channel, configBlob string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO notification_channels
		(org_id, name, kind, config, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, name) DO UPDATE SET kind = excluded.kind, config = excluded.config
		RETURNING id`,
		ch.OrgID, ch.Name, string(ch.Kind), nullIfEmpty(configBlob), nowStr()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create channel: %w", err)
	}
	return id, nil
}

// GetChannel returns the channel and its raw (encrypted) config blob scoped to
// orgID, or ErrNotFound.
func (s *PostgresStore) GetChannel(ctx context.Context, orgID, id int64) (notify.Channel, string, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+channelColumns+` FROM notification_channels WHERE org_id = $1 AND id = $2`, orgID, id)
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
func (s *PostgresStore) ListChannels(ctx context.Context, orgID int64) ([]notify.Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+channelColumns+` FROM notification_channels WHERE org_id = $1 ORDER BY id`, orgID)
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
func (s *PostgresStore) DeleteChannel(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM notification_channels WHERE org_id = $1 AND id = $2`, orgID, id)
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	return nil
}

// --- notification rules --------------------------------------------------
//
// ruleColumns and scanRule are shared with the SQLite backend (defined in
// sqlite.go), since both store the same row shapes. These methods differ only in
// $N placeholders.

// CreateRule upserts a rule by (org, channel, event_type, job_id) and returns
// its row ID via RETURNING id. The idempotent ON CONFLICT update (only enabled
// changes) lets config sync re-apply a rule without creating duplicates;
// created_at is preserved on update.
func (s *PostgresStore) CreateRule(ctx context.Context, r notify.Rule) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO notification_rules
		(org_id, channel_id, event_type, enabled, created_at, job_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (org_id, channel_id, event_type, job_id) DO UPDATE SET enabled = excluded.enabled
		RETURNING id`,
		r.OrgID, r.ChannelID, r.EventType, boolToInt(r.Enabled), nowStr(), r.JobID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create rule: %w", err)
	}
	return id, nil
}

// ListRules returns all rules for an org ordered by ID.
func (s *PostgresStore) ListRules(ctx context.Context, orgID int64) ([]notify.Rule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM notification_rules WHERE org_id = $1 ORDER BY id`, orgID)
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
func (s *PostgresStore) DeleteRule(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM notification_rules WHERE org_id = $1 AND id = $2`, orgID, id)
	if err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	return nil
}

// MatchingRules returns the enabled rules for orgID whose event_type matches and
// whose job_id is either 0 (all jobs) or equal to jobID (scoped to this event's
// job).
func (s *PostgresStore) MatchingRules(ctx context.Context, orgID int64, eventType string, jobID int64) ([]notify.Rule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM notification_rules
		 WHERE org_id = $1 AND enabled = 1 AND event_type = $2 AND (job_id = 0 OR job_id = $3)
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

// EmitEvent appends an event and returns its ID (RETURNING id).
func (s *PostgresStore) EmitEvent(ctx context.Context, e core.Event) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("emit event begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	id, err := pgEmitEventTx(ctx, tx, e)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("emit event commit: %w", err)
	}
	return id, nil
}

// ListEvents returns up to limit events for an org, newest first.
func (s *PostgresStore) ListEvents(ctx context.Context, orgID int64, limit int) ([]core.Event, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, type, source, payload, dedup_key, created_at
		 FROM events WHERE org_id = $1 ORDER BY id DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// GetHeartbeat returns the heartbeat with id for orgID, or ErrNotFound. Mirrors
// the SQLite backend (shared heartbeatColumns/scanHeartbeat).
func (s *PostgresStore) GetHeartbeat(ctx context.Context, orgID, id int64) (heartbeat.Heartbeat, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+heartbeatColumns+` FROM heartbeats WHERE org_id = $1 AND id = $2`, orgID, id)
	hb, err := scanHeartbeat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return heartbeat.Heartbeat{}, ErrNotFound
	}
	if err != nil {
		return heartbeat.Heartbeat{}, fmt.Errorf("get heartbeat: %w", err)
	}
	return hb, nil
}

// ListHeartbeatEvents mirrors the SQLite backend: a heartbeat's transition
// events by name (source='heartbeat', payload=name) newest first, bounded.
func (s *PostgresStore) ListHeartbeatEvents(ctx context.Context, orgID int64, name string, limit int) ([]core.Event, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, org_id, type, source, payload, dedup_key, created_at
		 FROM events WHERE org_id = $1 AND source = 'heartbeat' AND payload = $2
		 ORDER BY id DESC LIMIT $3`, orgID, name, limit)
	if err != nil {
		return nil, fmt.Errorf("list heartbeat events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// --- heartbeats ----------------------------------------------------------

// CreateHeartbeat upserts a heartbeat by (org, name), returning its row ID and
// the effective token. Mirrors the SQLite backend: hb.Token is consumed ONLY on
// insert; on conflict the existing token is preserved (absent from the SET list)
// and RETURNING token yields whichever token applies. period/grace are refreshed
// to the config values; status starts 'new' on insert, preserved on conflict.
func (s *PostgresStore) CreateHeartbeat(ctx context.Context, hb heartbeat.Heartbeat) (int64, string, error) {
	var (
		id    int64
		token string
	)
	err := s.db.QueryRowContext(ctx, `INSERT INTO heartbeats
		(org_id, name, token, period_seconds, grace_seconds, status, last_seen_at, created_at)
		VALUES ($1, $2, $3, $4, $5, 'new', NULL, $6)
		ON CONFLICT (org_id, name) DO UPDATE SET
			period_seconds = excluded.period_seconds,
			grace_seconds = excluded.grace_seconds
		RETURNING id, token`,
		hb.OrgID, hb.Name, hb.Token, hb.PeriodSeconds, hb.GraceSeconds, nowStr()).Scan(&id, &token)
	if err != nil {
		return 0, "", fmt.Errorf("create heartbeat: %w", err)
	}
	return id, token, nil
}

// GetHeartbeatByName mirrors the SQLite backend (shared heartbeatColumns/
// scanHeartbeat). Returns ErrNotFound when absent.
func (s *PostgresStore) GetHeartbeatByName(ctx context.Context, orgID int64, name string) (heartbeat.Heartbeat, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+heartbeatColumns+` FROM heartbeats WHERE org_id = $1 AND name = $2`, orgID, name)
	hb, err := scanHeartbeat(row)
	if errors.Is(err, sql.ErrNoRows) {
		return heartbeat.Heartbeat{}, ErrNotFound
	}
	if err != nil {
		return heartbeat.Heartbeat{}, fmt.Errorf("get heartbeat by name: %w", err)
	}
	return hb, nil
}

// ListHeartbeats returns all heartbeats for an org ordered by ID. Mirrors the
// SQLite backend (heartbeatColumns/scanHeartbeat are shared, defined in sqlite.go).
func (s *PostgresStore) ListHeartbeats(ctx context.Context, orgID int64) ([]heartbeat.Heartbeat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+heartbeatColumns+` FROM heartbeats WHERE org_id = $1 ORDER BY id`, orgID)
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

// RecordPing stamps last_seen_at and sets status to 'up' for the heartbeat
// identified by token, returning whether this was a down->up recovery along with
// the heartbeat's org and name. It runs in a transaction so the status read and
// the update are consistent. ErrNotFound is returned when no heartbeat owns the
// token. Mirrors the SQLite backend.
func (s *PostgresStore) RecordPing(ctx context.Context, token string, now time.Time) (int64, string, bool, error) {
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
	// FOR UPDATE locks the row for the tx so two concurrent pings can't both
	// observe 'down' and each emit a duplicate heartbeat.recovered. (SQLite is
	// already serialized by MaxOpenConns(1).)
	err = tx.QueryRowContext(ctx,
		`SELECT org_id, name, status FROM heartbeats WHERE token = $1 FOR UPDATE`, token,
	).Scan(&orgID, &name, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, ErrNotFound
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("record ping select: %w", err)
	}
	recovered := status == "down"

	if _, err := tx.ExecContext(ctx,
		`UPDATE heartbeats SET last_seen_at = $1, status = 'up' WHERE token = $2`,
		formatTime(now), token,
	); err != nil {
		return 0, "", false, fmt.Errorf("record ping update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, "", false, fmt.Errorf("record ping commit: %w", err)
	}
	return orgID, name, recovered, nil
}

// DueHeartbeats returns the 'up' heartbeats whose period+grace deadline has
// strictly passed at now. The deadline filter runs in Go (dueFromCandidates),
// so it is identical to the SQLite backend; only the SELECT differs ($N is moot
// here as it takes no parameters). heartbeatColumns, scanHeartbeat and
// dueFromCandidates are shared with the SQLite backend (defined in sqlite.go).
func (s *PostgresStore) DueHeartbeats(ctx context.Context, now time.Time) ([]heartbeat.Heartbeat, error) {
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
func (s *PostgresStore) SetHeartbeatStatus(ctx context.Context, id int64, status string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE heartbeats SET status = $1 WHERE id = $2`, status, id); err != nil {
		return fmt.Errorf("set heartbeat status: %w", err)
	}
	return nil
}

// SetHeartbeatStatusIf conditionally transitions a heartbeat's status from
// fromStatus to toStatus, but only when both the current status AND last_seen_at
// match the observed values. Returns (true, nil) when the row was updated, and
// (false, nil) when the guard rejected the update (a concurrent ping changed
// last_seen_at or the status no longer matches). Mirrors the SQLite backend.
func (s *PostgresStore) SetHeartbeatStatusIf(ctx context.Context, id int64, fromStatus, toStatus string, lastSeenAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE heartbeats SET status = $1 WHERE id = $2 AND status = $3 AND last_seen_at = $4`,
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
// ErrNotFound for a foreign/absent id. Mirrors ListRuns' row scan
// (runColumns/scanRun, shared from sqlite.go), filtered by (org_id, id).
func (s *PostgresStore) GetRun(ctx context.Context, orgID, runID int64) (jobs.Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM job_runs WHERE org_id = $1 AND id = $2`, orgID, runID)
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
// path by construction. scanSecretMeta is shared with the SQLite backend.
func (s *PostgresStore) ListSecrets(ctx context.Context, orgID int64) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, created_at FROM secrets WHERE org_id = $1 ORDER BY name`, orgID)
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

// --- auth: users ---------------------------------------------------------
//
// userColumns/scanUser, membershipColumns/scanMembership and
// tokenListColumns/scanTokenMeta are shared with the SQLite backend (defined in
// sqlite.go), since both store the same row shapes. These methods differ only in
// $N placeholders and RETURNING id.

// CreateUser inserts a new user and returns its ID via RETURNING id. The
// UNIQUE(org_id, email) constraint rejects a duplicate email within an org.
func (s *PostgresStore) CreateUser(ctx context.Context, u auth.User) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO users (org_id, email, password_hash, created_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		u.OrgID, u.Email, nullIfEmpty(u.PasswordHash), nowStr()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

// GetUserByEmail returns the user with the given email scoped to orgID, or
// ErrNotFound.
func (s *PostgresStore) GetUserByEmail(ctx context.Context, orgID int64, email string) (auth.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE org_id = $1 AND email = $2`, orgID, email)
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
func (s *PostgresStore) GetUserByID(ctx context.Context, orgID, id int64) (auth.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE org_id = $1 AND id = $2`, orgID, id)
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

// CreateMembership inserts a new membership and returns its ID via RETURNING id.
func (s *PostgresStore) CreateMembership(ctx context.Context, m auth.Membership) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO memberships (org_id, user_id, role, created_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		m.OrgID, m.UserID, m.Role, nowStr()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create membership: %w", err)
	}
	return id, nil
}

// GetMembership returns the membership for (orgID, userID), including the role,
// or ErrNotFound.
func (s *PostgresStore) GetMembership(ctx context.Context, orgID, userID int64) (auth.Membership, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+membershipColumns+` FROM memberships WHERE org_id = $1 AND user_id = $2`, orgID, userID)
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

// CreateToken inserts a new API token (storing only its hash) and returns its ID
// via RETURNING id.
func (s *PostgresStore) CreateToken(ctx context.Context, t auth.APIToken) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO api_tokens (org_id, name, token_hash, created_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		t.OrgID, t.Name, t.TokenHash, nowStr()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create token: %w", err)
	}
	return id, nil
}

// AuthenticateToken looks up a token strictly by its full hash (exact match) and
// returns the owning org ID and the token name. A miss yields ErrNotFound - no
// info leak, no partial match.
func (s *PostgresStore) AuthenticateToken(ctx context.Context, tokenHash string) (int64, string, error) {
	var (
		orgID int64
		name  string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, name FROM api_tokens WHERE token_hash = $1`, tokenHash).Scan(&orgID, &name)
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
func (s *PostgresStore) ListTokens(ctx context.Context, orgID int64) ([]auth.APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+tokenListColumns+` FROM api_tokens WHERE org_id = $1 ORDER BY id`, orgID)
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
func (s *PostgresStore) DeleteToken(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM api_tokens WHERE org_id = $1 AND id = $2`, orgID, id)
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return nil
}
