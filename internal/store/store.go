// Package store defines the persistence contract for Tend and its SQLite
// implementation. The Store interface is the only database contract the rest of
// the application depends on; concrete backends (SQLite now, Postgres later)
// implement it.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/notify"
)

// ErrNotFound is returned by Get* methods when no matching row exists.
var ErrNotFound = errors.New("store: not found")

// ErrCompactTooLarge is returned by ReclaimSpace when compacting the database
// would mean rewriting a file too large to rewrite while serving. Stop Tend
// and run `PRAGMA auto_vacuum = INCREMENTAL; VACUUM;` on the file once; from
// then on the sweep frees space in short steps by itself.
var ErrCompactTooLarge = errors.New("store: database too large to compact while serving; stop tend and run sqlite3 <db> 'PRAGMA auto_vacuum = INCREMENTAL; VACUUM;' once")

// SecretMeta is the non-secret metadata of a stored secret. It deliberately has
// NO ciphertext/value field so a listing of secrets can never carry secret
// material; ListSecrets selects only name and created_at.
type SecretMeta struct {
	Name      string
	CreatedAt time.Time
}

// Open returns a Store for the named driver opened at dsn. Supported drivers are
// "sqlite" and "postgres". The returned store is not yet migrated; call Migrate.
func Open(driver, dsn string) (Store, error) {
	switch driver {
	case "sqlite":
		return OpenSQLite(dsn)
	case "postgres":
		return OpenPostgres(dsn)
	default:
		return nil, fmt.Errorf("store: unknown driver %q", driver)
	}
}

// Store is the persistence contract used throughout Tend. All methods take a
// context as their first argument and are tenant-scoped via org IDs where
// applicable.
type Store interface {
	// lifecycle
	Close() error

	// schema
	Migrate(ctx context.Context) error

	// tenancy
	BootstrapDefaultOrg(ctx context.Context) (core.Org, error) // idempotent: returns existing or creates "default"
	// The org's ping key turns on the slug form of the ping API,
	// /ping/<ping-key>/<heartbeat-name>. SetOrgPingKey sets it ("" clears it;
	// a key another org holds is refused), GetOrgPingKey returns it ("" when
	// none), and OrgByPingKey resolves one to its org (ErrNotFound if none).
	SetOrgPingKey(ctx context.Context, orgID int64, key string) error
	GetOrgPingKey(ctx context.Context, orgID int64) (string, error)
	OrgByPingKey(ctx context.Context, key string) (int64, error)

	// jobs
	CreateJob(ctx context.Context, j jobs.Job) (int64, error)                     // returns new job ID
	GetJob(ctx context.Context, orgID, id int64) (jobs.Job, error)                // ErrNotFound if absent
	GetJobByName(ctx context.Context, orgID int64, name string) (jobs.Job, error) // ErrNotFound if absent
	ListJobs(ctx context.Context, orgID int64) ([]jobs.Job, error)
	UpdateJob(ctx context.Context, j jobs.Job) error
	// DeleteJob removes a job and ALL of its dependent rows (its job_runs and any
	// notification_rules scoped to it) in a single transaction, then the job row,
	// and records job.removed (jobs.RemovedEvent) in that transaction, before the
	// job-scoped rules are gone so they can still route it.
	// Deletes are explicit (not FK cascade) so behavior is identical on every
	// backend. All-jobs rules (job_id = 0) are preserved. ErrNotFound if the job
	// doesn't exist for the org (no side effect: the transaction is rolled back).
	DeleteJob(ctx context.Context, orgID, id int64) error
	DueJobs(ctx context.Context, now time.Time) ([]jobs.Job, error) // enabled jobs with NextRun <= now AND no pending/running run

	// run queue + history
	EnqueueRun(ctx context.Context, orgID, jobID int64) (int64, error)   // inserts a 'pending' job_run, returns run ID
	ClaimRun(ctx context.Context, worker string) (jobs.Run, bool, error) // atomically claim one 'pending' run -> 'running'; ok=false if none
	// FinishRun records the terminal state of a 'running' run. A run that is no
	// longer 'running' (the reaper failed it first) is left untouched and
	// jobs.ErrRunNotRunning is returned. An absent id gives an error that is
	// both ErrNotFound and jobs.ErrRunGone.
	FinishRun(ctx context.Context, runID int64, status jobs.RunStatus, exitCode int, output string) error
	// FinishRunAndEmit atomically records the terminal run state (including the
	// final attempt count) AND appends the terminal lifecycle event in a single
	// transaction. This prevents the lost-event scenario where FinishRun commits
	// but EmitEvent fails. When the run succeeded and the job's previous
	// finished run (the one that ended last) did not, run.recovered is appended
	// in the same transaction too. Returns the new (terminal) event ID. Like FinishRun it returns
	// jobs.ErrRunNotRunning, writing neither the state nor the event, when the
	// run is no longer 'running', and ErrNotFound and jobs.ErrRunGone for an
	// absent id.
	FinishRunAndEmit(ctx context.Context, runID int64, status jobs.RunStatus, exitCode, attempt int, output string, ev core.Event) (int64, error)
	GetRun(ctx context.Context, orgID, runID int64) (jobs.Run, error)                // run detail incl. Output; ErrNotFound for a foreign/absent id
	ListRuns(ctx context.Context, orgID, jobID int64, limit int) ([]jobs.Run, error) // newest first
	RequeueOrphanedRuns(ctx context.Context) (int64, error)                          // reset serve's 'running' runs -> 'pending' (crash recovery), never a `tend run`'s; returns rows affected
	ListRunningRuns(ctx context.Context) ([]jobs.Run, error)                         // all 'running' runs (all orgs), for the reaper sweep
	// ReapStaleRun atomically fails a run still in 'running' and appends ev in the
	// same transaction (mirrors FinishRunAndEmit). false = run already terminal.
	ReapStaleRun(ctx context.Context, runID int64, output string, ev core.Event) (bool, error)

	// secrets (ciphertext in/out; encryption lives elsewhere)
	PutSecret(ctx context.Context, orgID int64, name, ciphertext string) error // upsert by (org,name)
	GetSecret(ctx context.Context, orgID int64, name string) (string, error)   // returns ciphertext; ErrNotFound if absent
	ListSecrets(ctx context.Context, orgID int64) ([]SecretMeta, error)        // names + created_at ONLY; NEVER ciphertext

	// auth: users
	CreateUser(ctx context.Context, u auth.User) (int64, error)                       // returns new user ID; UNIQUE(org_id,email) rejects dupes
	GetUserByEmail(ctx context.Context, orgID int64, email string) (auth.User, error) // org-scoped; ErrNotFound if absent
	GetUserByID(ctx context.Context, orgID, id int64) (auth.User, error)              // org-scoped; ErrNotFound if absent

	// auth: memberships
	CreateMembership(ctx context.Context, m auth.Membership) (int64, error)          // returns new membership ID
	GetMembership(ctx context.Context, orgID, userID int64) (auth.Membership, error) // org-scoped; returns the role; ErrNotFound if absent

	// auth: API tokens (only the hash is persisted; never read back through ListTokens)
	CreateToken(ctx context.Context, t auth.APIToken) (int64, error)                               // returns new token ID
	AuthenticateToken(ctx context.Context, tokenHash string) (orgID int64, name string, err error) // exact hash match; ErrNotFound on a miss
	ListTokens(ctx context.Context, orgID int64) ([]auth.APIToken, error)                          // org-scoped; TokenHash is ALWAYS empty (not selected)
	DeleteToken(ctx context.Context, orgID, id int64) error                                        // org-scoped delete

	// event pipeline
	EmitEvent(ctx context.Context, e core.Event) (int64, error)
	ListEvents(ctx context.Context, orgID int64, limit int) ([]core.Event, error) // newest first

	// retention pruning (the daily serve sweep). Each method hard-deletes rows
	// created strictly before the cutoff and returns the rows removed.
	// PruneEvents keeps events still referenced by a PENDING delivery (the
	// notify worker joins the event on claim; pruning it would break the
	// queue). PruneJobRuns keeps pending/running rows regardless of age (the
	// runner and the orphan reaper own their lifecycle). PruneDeliveries only
	// removes finalized rows (delivered or failed).
	PruneEvents(ctx context.Context, before time.Time) (int64, error)
	PruneJobRuns(ctx context.Context, before time.Time) (int64, error)
	PruneDeliveries(ctx context.Context, before time.Time) (int64, error)
	// PruneHeartbeatPings removes heartbeat ping log entries. (Each heartbeat
	// also keeps only its newest MaxPingsPerHeartbeat entries, trimmed as
	// pings arrive.)
	PruneHeartbeatPings(ctx context.Context, before time.Time) (int64, error)
	// ReclaimSpace gives the space pruning freed back to the file system when
	// there is enough of it to matter, and returns the bytes the database
	// shrank by. SQLite compacts (once switching a small file to incremental
	// auto-vacuum with VACUUM, then in short steps that let other queries
	// through, for a bounded time per call), returning ErrCompactTooLarge
	// instead of rewriting a large file while serving; Postgres does nothing,
	// as autovacuum already makes the space reusable.
	ReclaimSpace(ctx context.Context) (int64, error)

	// durable notification deliveries. Pending rows are enqueued transactionally
	// by every event insert (EmitEvent / FinishRunAndEmit / ReapStaleRun / ...)
	// for alertable event types with matching enabled rules whose channel
	// exists; the notify.Worker drains them. See notify.WorkerStore for the
	// claim/lease semantics.
	ClaimDueDeliveries(ctx context.Context, now time.Time, lease, hold time.Duration, limit int) ([]notify.Delivery, error)
	MarkDeliveryDelivered(ctx context.Context, id int64) error
	RescheduleDelivery(ctx context.Context, id int64, nextAttemptAt time.Time) error
	FailDelivery(ctx context.Context, id int64) error
	// FailDeliveryAndEmit finalizes a still-pending delivery as failed and
	// appends ev (notification.failed) in the same transaction; a delivery
	// that is no longer pending is left alone and ev is not written.
	FailDeliveryAndEmit(ctx context.Context, id int64, ev core.Event) error

	// notification channels (encrypted config blob in/out; encryption lives elsewhere)
	CreateChannel(ctx context.Context, ch notify.Channel, configBlob string) (int64, error) // upsert by (org,name); returns row ID
	GetChannel(ctx context.Context, orgID, id int64) (notify.Channel, string, error)        // channel + encrypted blob; ErrNotFound if absent
	ListChannels(ctx context.Context, orgID int64) ([]notify.Channel, error)                // metadata only (no blob)
	DeleteChannel(ctx context.Context, orgID, id int64) error                               // org-scoped delete; also deletes its rules and fails its pending deliveries

	// notification rules (event_type -> channel bindings; job_id 0 = all jobs)
	CreateRule(ctx context.Context, r notify.Rule) (int64, error)                                         // upsert by (org,channel,event,job); returns row ID
	ListRules(ctx context.Context, orgID int64) ([]notify.Rule, error)                                    // org-scoped
	DeleteRule(ctx context.Context, orgID, id int64) error                                                // org-scoped delete
	MatchingRules(ctx context.Context, orgID int64, eventType string, jobID int64) ([]notify.Rule, error) // enabled; event_type matches; job_id 0 (all) or = jobID

	// heartbeats
	// CreateHeartbeat upserts a heartbeat by (org_id, name) and returns its row ID
	// plus the EFFECTIVE token. hb.Token (a freshly generated token) is used ONLY
	// on insert; on conflict the existing token is preserved and returned, so a
	// re-sync keeps the same ping URL. status starts 'new' on insert and is
	// preserved on conflict; period/grace are always updated to the config values.
	CreateHeartbeat(ctx context.Context, hb heartbeat.Heartbeat) (id int64, token string, err error)
	// CreateHeartbeatIfAbsent inserts hb (status 'new') unless the org already
	// has a heartbeat of that name, which it leaves exactly as it is. It
	// returns the token now in effect and whether hb was created. A slug
	// ping's ?create=1 uses it, so it can never reset an existing heartbeat.
	CreateHeartbeatIfAbsent(ctx context.Context, hb heartbeat.Heartbeat) (token string, created bool, err error)
	// ListHeartbeats returns all heartbeats for an org ordered by ID.
	ListHeartbeats(ctx context.Context, orgID int64) ([]heartbeat.Heartbeat, error)
	// GetHeartbeatByName returns the heartbeat named name for orgID, or
	// ErrNotFound. It includes the ping token so the CLI can recover the ping URL
	// of a config-as-code heartbeat.
	GetHeartbeatByName(ctx context.Context, orgID int64, name string) (heartbeat.Heartbeat, error)
	// GetHeartbeat returns the heartbeat with id for orgID, or ErrNotFound.
	GetHeartbeat(ctx context.Context, orgID, id int64) (heartbeat.Heartbeat, error)
	// ListHeartbeatEvents returns a heartbeat's transition events (heartbeat.missed
	// / heartbeat.recovered, keyed by name in the events table), newest first,
	// bounded by limit.
	ListHeartbeatEvents(ctx context.Context, orgID int64, name string, limit int) ([]core.Event, error)
	// DeleteHeartbeat removes the heartbeat with id for orgID, or ErrNotFound,
	// with its ping log, and records heartbeat.removed in the same transaction.
	// Its past events (the audit trail) are intentionally left in place.
	DeleteHeartbeat(ctx context.Context, orgID, id int64) error
	// RecordPing records a dead-man's-switch ping for the heartbeat identified by
	// the globally-unique token: it stamps last_seen_at and sets status to 'up'.
	// It returns ErrNotFound if no heartbeat owns the token. recovered is true iff
	// this ping moved the heartbeat from 'down' to 'up' (decided by
	// compare-and-set, so of concurrent pings exactly one recovers it); a ping on
	// a 'new' or already-'up' heartbeat is not a recovery. On a recovery the
	// heartbeat.recovered event and its deliveries are appended in the same
	// transaction. orgID and name identify the heartbeat for the caller's
	// dispatch nudge.
	RecordPing(ctx context.Context, token string, now time.Time) (orgID int64, name string, recovered bool, err error)
	// RecordHeartbeatPing records one ping of the Healthchecks-compatible
	// ping API for the heartbeat that owns token (ErrNotFound if none), and
	// logs it in the heartbeat's ping log with its exit status, rid, body and,
	// for a completion paired with its start, the run's duration.
	//   - success: stamps last_seen_at and sets 'up'; a 'down' heartbeat
	//     recovers (heartbeat.recovered). It ends the run being timed when it
	//     carries that run's rid or none.
	//   - fail: stamps last_seen_at and sets 'down' at once; a heartbeat that
	//     was not down records heartbeat.missed with a heartbeat.FailedEvent
	//     detail. It ends any run.
	//   - start: starts timing a run (started_at, started_rid); status and
	//     last_seen_at are untouched.
	//   - log: changes nothing.
	// The state change, the log entry and the event (with its deliveries) are
	// one transaction, and of concurrent pings exactly one records a
	// transition. The result names the heartbeat and the event, if any, for
	// the caller's dispatch nudge.
	RecordHeartbeatPing(ctx context.Context, token string, in heartbeat.PingInput, now time.Time) (heartbeat.PingResult, error)
	// ListHeartbeatPings returns the ping log of the heartbeat with id for
	// orgID, newest first, bounded by limit (a default when <= 0).
	ListHeartbeatPings(ctx context.Context, orgID, heartbeatID int64, limit int) ([]heartbeat.Ping, error)
	// DueHeartbeats returns the watched heartbeats whose period+grace deadline has
	// strictly passed at now. The deadline is computed in Go (dialect-agnostic):
	// the query selects every 'up' heartbeat with a non-NULL last_seen_at (anchored
	// on last_seen_at) plus every 'new' heartbeat never pinged (anchored on
	// created_at, arming the dead-man's switch for an unpinged monitor), and the
	// filter is applied per row. 'down' heartbeats are never watched.
	DueHeartbeats(ctx context.Context, now time.Time) ([]heartbeat.Heartbeat, error)
	// SetHeartbeatStatus updates a heartbeat's status (e.g. 'up' -> 'down') by ID.
	SetHeartbeatStatus(ctx context.Context, id int64, status string) error
	// SetHeartbeatStatusIf conditionally transitions a heartbeat's status from
	// fromStatus to toStatus only when the current status AND last_seen_at both
	// match the observed values. Returns (true, nil) when the row was updated;
	// (false, nil) when the guard rejected it (concurrent ping or stale state).
	// Used by the watcher to close the watcher↔ping race.
	SetHeartbeatStatusIf(ctx context.Context, id int64, fromStatus, toStatus string, lastSeenAt time.Time) (bool, error)
	// SetHeartbeatStatusIfAndEmit makes the same guarded transition and, only
	// when it fires, appends ev (enqueueing its deliveries) in the same
	// transaction, so a heartbeat is never marked 'down' without its
	// heartbeat.missed. (false, nil) when the guard rejected it; nothing is
	// written then or on any error.
	SetHeartbeatStatusIfAndEmit(ctx context.Context, id int64, fromStatus, toStatus string, lastSeenAt time.Time, ev core.Event) (bool, error)
	// MarkHeartbeatDownAndEmit is the watcher's transition: it moves a
	// heartbeat observed by DueHeartbeats to 'down', ends any run it was
	// timing, and appends ev with its deliveries, in one transaction, but only
	// while its status, last_seen_at and started_at still match observed. A
	// ping or a /start that came in meanwhile wins: (false, nil), and nothing
	// is written then or on any error.
	MarkHeartbeatDownAndEmit(ctx context.Context, observed heartbeat.Heartbeat, ev core.Event) (bool, error)
}

// Compile-time assertion that *SQLiteStore implements Store.
var _ Store = (*SQLiteStore)(nil)
