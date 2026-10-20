# Architecture

This document describes the durable architecture of `tend` for contributors. It
explains how the code is organized, the seams that keep it decoupled, and the
invariants you must preserve when changing it. Read it before making structural
changes; the conventions here are load-bearing.

`tend` is licensed under **AGPL-3.0** (see `LICENSE`). Contributions require
signing the CLA (`CLA.md`); the CLA-assistant check runs on every pull request.

---

## 1. Overview

`tend` is a self-hostable cron / job-runner with run monitoring, alerting, and
dead-man's-switch heartbeats, shipped as a **single static, CGO-free Go binary**.
Time-zone data is embedded (`_ "time/tzdata"` in `cmd/tend/main.go`), so cron
schedules resolve identically everywhere with no host `tzdata` dependency.

- **Modular monolith.** The application is split into small `internal/*` packages
  with explicit, one-directional dependencies, but compiles and ships as one
  binary. There is no network boundary between subsystems.
- **Storage.** SQLite is the default (pure-Go `modernc.org/sqlite` driver, no
  cgo); Postgres (`pgx` stdlib driver) is available for scale. Both back the
  exact same `store.Store` interface (see §3).
- **The `serve` process.** `tend serve` runs everything off **one clock and one
  notification delivery worker**:
  - the **runner** (scheduler tick, worker goroutines that claim and execute
    runs, and a periodic sweep that fails runs whose worker was lost),
  - the **HTTP server** (the heartbeat ping API, compatible with Healthchecks',
    `/healthz`, the read + action REST API, and the htmx dashboard),
  - the **heartbeat watcher** (periodic scan for missed dead-man's-switch pings),
  - the **delivery worker** (drains the durable notification queue),
  - the **retention sweep** (prunes old events, job runs, deliveries, and
    heartbeat ping logs at startup and then daily).

  The runner, HTTP server, and watcher share the injected `clock.Clock` and a
  single `func(context.Context, core.Event)` closure that nudges the delivery
  worker, so behavior is consistent and testable (a `clock.FakeClock` drives
  time in tests).

  `serve` refuses to start without `TEND_MASTER_KEY` (fail closed): channel
  configs are encrypted with it, so without it nothing could be alerted on,
  and the dashboard signs its sessions with it. `--no-master-key` is the
  explicit opt-out for a local experiment; it starts without the delivery
  worker and the dashboard and says so in the banner.

Other subcommands (`sync`, `job`, `run`, `logs`, `secret`, `channel`, `rule`,
`heartbeat`, `import`, `doctor`, `user`, `token`, `version`) are one-shot CLI
operations dispatched from `internal/cli`. `exec` is the odd one out: it wraps a
command with pings to a ping URL (`internal/pingexec`) and never opens a
database, so it runs on any host that can reach Tend.

---

## 2. Package layout

All application code lives under `internal/` (so nothing is importable as a public
API). `cmd/tend/main.go` is the entry point: it loads config, installs a
SIGINT/SIGTERM-cancelled context, and calls `cli.Run`.

| Package | Responsibility |
| --- | --- |
| `internal/core` | Cross-cutting domain types shared by everything: `Org` (tenant) and the generic `Event` record (the event-pipeline spine). |
| `internal/store` | The `Store` interface (single persistence seam) plus the SQLite and Postgres implementations and embedded SQL migrations. |
| `internal/jobs` | Job/Run domain types, scheduling (`Job.NextRun`, `ValidateCron`), the `Executor` (shell + HTTP, the `tls`, `domain` and `expiry` probes and the restore `drill`, with capped output), the type-specific settings (`Spec`, `ValidateSpec`), and the `Runner` loop (scheduler + workers + stale-run reaper + secret resolution + output redaction). |
| `internal/auth` | The identity types `Principal`, `User`, `APIToken`, `Membership`, and the `tend_` prefix of the API tokens Tend issues. |
| `internal/notify` | The notification domain: channels, rules, the `Alertable` filter, how each event is rendered, and the wiring of kit's delivery worker over the store's deliveries queue (`NewWorker`: the queue, the send step, the give-up that records `notification.failed`). |
| `internal/heartbeat` | The `Heartbeat` domain type with its `Deadline` (a started run brings it forward), the ping kinds and ping log types, the detail a `heartbeat.missed` carries when it is a failure or an overrun, and the `Watcher` that marks missed heartbeats down. |
| `internal/pingexec` | `tend exec`: run a command between a start ping and an exit-status ping carrying its output's tail, with retries, signal forwarding and the command's exit status. Talks to any Healthchecks-compatible ping URL; no store. |
| `internal/hcimport` | `tend import healthchecks`: read a project's checks from the Healthchecks Management API (v3) and plan the heartbeats they become (UUID as token, cron schedules approximated by a period). |
| `internal/configfile` | YAML config-as-code: `Parse` (file → structs) and `Reconcile` (structs → DB, one-way). |
| `internal/httpserver` | The HTTP surface: `requireAuth` middleware, the read + action REST API with leak-free DTOs, the htmx dashboard, login/logout, the Healthchecks-compatible ping API (`ping.go`: token and slug forms, body capture) and the health endpoint, and how the security headers and the `/login` and `/ping` rate limits from `kit/httpx` are applied. |
| `internal/config` | Environment-driven process configuration (driver, DSN, master key). `TEND_DB` has no default; an unset value leaves the DSN empty and `cli.Run` refuses to open a database. |
| `internal/cli` | Subcommand dispatch and the `serve` wiring that ties the runner, HTTP server, watcher, delivery worker, and retention sweep together. |

Code the Marsad tools share comes from the
[`github.com/marsadhq/kit`](https://github.com/marsadhq/kit) module, pinned in
`go.mod`; Tend keeps its own domain and imports these:

| Package | What Tend uses it for |
| --- | --- |
| `kit/clock` | The `Clock` interface, `RealClock`, and a concurrency-safe `FakeClock` for tests. |
| `kit/secrets` | The AES-256-GCM `Box` used to encrypt/decrypt secret values and channel config. |
| `kit/auth` | Cryptographic identity primitives: argon2id passwords, API token generation and hashing, signed session cookies, CSRF tokens. |
| `kit/httpx` | The security-headers middleware (the CSP), the HTTP server with every connection phase bounded, and the keyed token-bucket rate limiter with its client-address keying. |
| `kit/notify` | The `Provider` contract and `Message`, the providers (webhook, Slack, Discord, SMTP, Telegram, ntfy, Pushover), `BuildProvider` from a channel's config, permanent-error classification, and the durable delivery `Worker` with its leases, backoff and give-up. |
| `kit/event` | `TendWebhook`, the body Tend's webhook channel posts, the `marsad.event/v1` envelope (`V1`) Tend adds to it under `marsad` (built in `internal/notify/message.go`, named by `TEND_INSTANCE`), and the golden fixtures that pin both byte for byte. |
| `kit/urlredact` | Stdlib-only helpers that reduce a URL to its host and a request error to its cause, used by the notification providers and the HTTP job executor so secret-bearing URLs stay out of logs and run output. |

---

## 3. The store interface and SQLite/Postgres parity

`store.Store` (`internal/store/store.go`) is the **single persistence contract**
the rest of the application depends on. `store.Open(driver, dsn)` returns the
right backend; both `*SQLiteStore` and `*PostgresStore` satisfy `Store`
(enforced by compile-time `var _ Store = (*…)(nil)` assertions).

### Consumer-defined interfaces (no import cycles)

The dependency direction is deliberate and strict: **`store` imports the domain
packages (`jobs`, `notify`, `heartbeat`, `auth`, `core`); those packages never
import `store`.** To still talk to persistence, each consumer package defines its
*own* small interface describing only the methods it needs, and the concrete
store types satisfy it **structurally**. Examples:

- `jobs.RunnerStore`: what the runner needs (`DueJobs`, `ClaimRun`,
  `FinishRunAndEmit`, …).
- `notify.WorkerStore` / `notify.ChannelStore`: what the delivery worker and
  channel helpers need.
- `heartbeat.WatchStore`: what the watcher needs.
- `configfile.ReconcileStore`: what reconcile needs.

This is the idiom that keeps the graph acyclic while still letting `store` return
rich domain types. When you add a store method, add it to `Store` *and* to any
consumer interface that needs it; never make a domain package import `store`.

### Both backends, identical behavior

The two backends are kept behaviorally identical, differing only where the SQL
dialects force it:

- **Dialect placeholders.** SQLite uses `?`; Postgres uses `$N` and
  `RETURNING id` (the pgx stdlib driver does not implement
  `Result.LastInsertId`).
- **Shared `…Columns` consts + `scan…` helpers.** Each table has a column-list
  constant (e.g. `jobColumns`, `runColumns`, `heartbeatColumns`) read **by
  ordinal position** by a `scan…` helper. The order is load-bearing: it must
  match the column order in *both* `migrations/sqlite/*.sql` and
  `migrations/postgres/*.sql`. The value/scan helpers in `sqlite.go` (timestamp,
  env-JSON, bool/null helpers) are reused unchanged by `postgres.go`, since both
  store data in the same shapes (timestamps as TEXT, flags/counters as INTEGER,
  env as JSON TEXT).
- **Fixed-width UTC timestamps for lexical ordering.** All timestamps are stored
  with `tsLayout = "2006-01-02T15:04:05.000000000Z07:00"`: always UTC ("Z"),
  always 9 fractional digits. This guarantees that byte-wise string comparison
  matches chronological order, which `DueJobs` relies on when it compares the
  TEXT `next_run` column with `<= ?`. (`time.RFC3339Nano` omits trailing
  fractional zeros and would break ordering at sub-second boundaries.)
- **Type-specific settings as JSON.** A probe's or a drill's settings
  (`jobs.Spec`) are stored as JSON in `jobs.spec` (migration 0012), written and
  read by the same `encodeSpec` / `decodeSpec` on both backends, like `env`.
  Shell and http jobs keep it NULL, so their rows are exactly what they were.
- **`ErrNotFound`.** `Get*` methods return the sentinel `store.ErrNotFound` when
  no row matches; callers use `errors.Is`.
- **Explicit transactional deletes.** Cascading deletes are done in an explicit
  transaction, not via FK cascade, so behavior is identical on both backends.
  `DeleteJob` removes a job's `job_runs` and its job-scoped
  `notification_rules`, then the job row, in one transaction (preserving
  all-jobs rules where `job_id = 0`); it returns `ErrNotFound` (rolling back)
  when the job is absent.
- **Atomic finish + emit.** `FinishRunAndEmit` writes the terminal run state and
  the terminal lifecycle event in a single transaction (see §4).
- **Atomic emit + enqueue.** Every event insert goes through one helper per
  backend (`emitEventTx` / `pgEmitEventTx`), which also inserts the pending
  `deliveries` rows for that event on the same transaction (see §4).
- **Retention pruning.** `PruneEvents`, `PruneJobRuns`, and `PruneDeliveries`
  hard-delete rows created before a cutoff. They never touch pending or
  running runs, pending deliveries, or an event that a pending delivery still
  references. `ReclaimSpace` gives freed space back afterwards (SQLite only;
  see §8).

The claim path differs subtly by engine: SQLite serializes writes at the pool
(`SetMaxOpenConns(1)`) and uses a single atomic `UPDATE … RETURNING` to claim the
oldest pending run; Postgres allows real connection concurrency so `FOR UPDATE
SKIP LOCKED`-style claiming is meaningful. Both guarantee no two workers claim the
same run.

Crash recovery is at-least-once: `RequeueOrphanedRuns` resets the `running`
rows serve's own runner claimed back to `pending` at startup (serve is
single-instance, so such a row found at boot was orphaned by a crash). A run a
one-off `tend run` claimed is stamped `claimed_by = run:<host>:<pid>`
(`jobs.ManualClaimant`) and left alone: that process may still be executing
it, and requeueing it would run the job twice. Should the `tend run` process
have died, the reaper below fails the run once its time limit has passed.

While `serve` is up, the runner's reaper (`Runner.ReapOnce`, every minute)
covers the other case: a run whose worker was lost without the process
restarting. `started_at` is stamped once, when the run is claimed, while the
executor may spend `MaxRetries + 1` attempts on it, so the reaper waits for the
longest the whole run can take (`Executor.maxRunDuration`: every attempt
reaching the job's timeout, `jobs.DefaultTimeout` of 30 minutes when the job
sets none, with its kill grace, plus the backoff between attempts) and two
minutes of slack on top. A run still `running` after that is presumed orphaned
and failed through `ReapStaleRun`. Like `FinishRunAndEmit`, that writes the
terminal state and the `run.failed` event in one transaction. Without it the
orphan would hold the job's no-overlap guard until the next restart.

Which definition of the job that limit is computed from depends on who is
executing the run. The runner keeps an in-memory registry of the runs its own
workers are executing, each with the limit taken from the definition the worker
loaded at claim, which is the one the executor enforces to the end. The reaper
judges those runs by that claim-time limit, so lowering `timeout_seconds` or
`max_retries` (`tend sync`, a job edit) while a run is in flight cannot get a
healthy run failed; a registered run is reaped only once its claim-time
deadline has passed. A run that is not in the registry was claimed by another
process (`tend run`) or lost its worker, and is judged by the job as it is now.

A finish that fails is retried. Until `FinishRunAndEmit` commits, the result of
a run exists only in its worker's memory, so on an error (a locked SQLite
database, a full disk) the runner tries again after 250 ms, 1 s and 4 s, four
attempts in all, logging each failure. Only when the last one fails as well is
the run left `running`, to be requeued at the next start or failed by the
reaper. Two errors are not retried, because no retry can change them:
`jobs.ErrRunNotRunning` (below) and `jobs.ErrRunGone`, a run that no longer
exists because its job was deleted while it was in flight. That result is
dropped and logged, and the error is still returned, so `tend run` exits
non-zero as it always has. Shutdown ends the attempts at once; the run stays
`running` to be requeued at the next start, which is logged with the run and
its job, since the job will then be executed a second time.

A terminal state is final in both directions. `ReapStaleRun` and the finish
path (`FinishRun`, `FinishRunAndEmit`) each update only a run that is still
`running`, so whichever commits first wins: reaping a run that just finished is
a no-op, and a worker that finishes a run the reaper already failed gets
`jobs.ErrRunNotRunning`, writes nothing, and emits no second terminal event.

None of this happens in silence. The runner logs through `Runner.Logger`, which
`serve` sets to its own logger and `tend run` points at stderr: a store error on
the scheduler tick, in the reaper, or on the claim/finish path at ERROR (a lost
`run.started`, being advisory, at WARN), each reaped run and each discarded
late result at WARN, and the number of runs requeued at startup at INFO. Errors
caused by shutdown cancelling the context are not logged. One error is not
logged every time it happens: idle workers poll for work about once a second,
so a claim that keeps failing is logged when it first fails, then at most once
a minute with the number of failed claims so far, and once more at INFO when a
claim works again.

Captured output is arbitrary bytes; what is stored is text. Before finishing a
run the runner (`storedOutput`) drops NUL bytes and replaces invalid UTF-8 with
U+FFFD, then redacts the job's secret values, then trims the result back to the
output cap. Cleaning comes before redaction on purpose: redaction is a literal
match, so it has to see the text as it will be stored, or a secret printed with
NUL bytes between its characters would slip past and be reassembled when the
NULs are dropped. A secret that is not valid UTF-8 is matched one step earlier
as well, byte for byte, once the NULs are gone and before its invalid bytes are
replaced. Both backends are handed the same valid text, so they store
identical bytes and the dashboard is never served invalid UTF-8. The Postgres
finish path still applies the same clean-up (`pgText`), because a `TEXT` column
accepts neither NUL nor invalid UTF-8 and a rejected value would leave the run
`running`; for output that came through the runner it changes nothing.

---

## 4. Event pipeline and the `EventSink` seam

`core.Event` is the generic record carried by the event pipeline: `{ID, OrgID,
Type, Source, Payload (JSON), DedupKey, CreatedAt, Detail}`. Events are the spine that ties
the runner, watcher, and notifier together. `Detail` is optional JSON for the
notification text only (why a heartbeat went down); it is stored with the event
but left out of the webhook's legacy `event` object, whose bytes never change.

Event types currently emitted:

- **Runs:** `run.started` (best-effort), `run.succeeded`, `run.failed`,
  `run.recovered`, for every job type: a probe that finds a certificate about
  to expire, or a drill whose check fails, is a failed run like a shell job
  that exits 1, and nothing downstream of the run knows the difference. Timeouts surface as `run.failed` with the precise status in
  the payload, so the `run.*` type vocabulary stays small. `run.recovered` is
  recorded next to `run.succeeded` (same payload, dedup key
  `run:<run_id>:run.recovered`) when the job's previous finished run had not
  succeeded: failed, timed out or reaped.
- **Heartbeats:** `heartbeat.missed`, `heartbeat.recovered`,
  `heartbeat.removed`. A heartbeat that was never pinged is armed from its
  creation time, so `heartbeat.missed` also covers a first ping that never
  arrives. It also covers a failure ping (`/fail`, an exit status of 1 to
  255), which takes the heartbeat down at once, and a run started with
  `/start` that does not finish within the grace (`Heartbeat.RunGrace`: the
  period when the grace is 0); its `Detail` says which
  (`heartbeat.FailedEvent`, `heartbeat.OverranEvent`), and the subject reads
  `Heartbeat failed: <name> (exit 3)` or `Heartbeat missed: <name> (run did
  not finish)`. Every heartbeat event carries the plain heartbeat name as its
  payload; there are no other heartbeat event types.
- **Removals:** `job.removed` (payload `{"job_id":…,"job_name":…}`, dedup key
  `job:<id>:removed`) and `heartbeat.removed` are recorded by the store's
  `DeleteJob` / `DeleteHeartbeat` in the delete's own transaction, so every
  way of deleting (`tend job rm`, `tend heartbeat rm`) reports it. A rule
  scoped to the deleted job still routes its `job.removed`: the event is
  written before the job's rules are deleted. (`tend sync` does not delete
  jobs or heartbeats; see section 5.)
- **Notifications:** `notification.failed` (recorded when a delivery is given
  up; the payload is the type of the event that was not delivered).

**Terminal run events are written atomically with the run.** The runner records
the terminal run state and appends the terminal event (and, for a recovery,
`run.recovered`) in one transaction via `FinishRunAndEmit`. This closes the lost-event gap where `FinishRun` could commit
but a separate `EmitEvent` then fail. (`run.started` is explicitly *not* terminal
and is best-effort; a lost start event is non-critical because the terminal
event is guaranteed.) A heartbeat's transitions get the same treatment: the
watcher marks a heartbeat `down` and records `heartbeat.missed` in one
transaction (`MarkHeartbeatDownAndEmit`, guarded on the status, last ping and
started run it saw), so a failed write leaves the heartbeat `up` and the next
check tries again instead of losing the alert. A ping is one transaction too
(`RecordHeartbeatPing`): the heartbeat's new state, its ping log entry, and the
`heartbeat.missed` of a failure or the `heartbeat.recovered` of a recovery
commit together or not at all, and of racing pings exactly one records the
transition. The ping log entry keeps the request body (cleaned to text, the
URL's credential redacted, capped at 10 kB by default and at most 100 kB,
`TEND_PING_BODY_KB`) and, for a completion paired with
its start by `?rid=`, the run's duration.

**Notifications are a durable queue.** Every event insert (`EmitEvent`,
`FinishRunAndEmit`, `ReapStaleRun`, `MarkHeartbeatDownAndEmit`, `RecordHeartbeatPing`) also inserts, in the same transaction, one
`pending` row in the `deliveries` table for each existing channel that has an
enabled rule matching the event: org-wide rules plus rules scoped to the
event's job. Deleting a channel deletes its rules and finalizes its pending
deliveries in the same transaction, so nothing is ever queued for a channel
that is gone.
Because the delivery commits atomically with the event, neither a crash nor a
slow destination can lose a matched notification. Overlapping rules for the
same channel produce a single delivery.

**Only alertable events enqueue.** Only the `alertable` set (`run.failed`,
`run.recovered`, `heartbeat.missed`, `heartbeat.recovered`, `job.removed`,
`heartbeat.removed`, `notification.failed`; see `notify.Alertable`) ever
enqueues deliveries, so emitters can record `run.started` and `run.succeeded`
freely. `notification.failed` is alertable so that a channel that stopped
working can be reported through another one. The loop guard is in the worker's
give-up (`GiveUp` on Tend's queue): when the delivery it gives up on is itself a `notification.failed`, it marks it
failed and records nothing further, so a broken channel cannot start a chain of
failure reports. A given-up delivery and its `notification.failed` are written
in one transaction (`FailDeliveryAndEmit`).

**`notify.Worker` drains the queue.** It is kit's `notify.Worker`, driving
Tend's deliveries through the `kit/notify` `Queue` interface. It claims due rows (`ClaimDueDeliveries`
increments the attempt count and pushes `next_attempt_at` out by a one-minute
lease, so a claim held by a crashed worker simply becomes due again), decrypts
the channel config, builds the provider, and sends. Success marks the row
`delivered`. Failure reschedules it with exponential backoff (1 second,
doubling, capped at 5 minutes). Once a delivery has been failing for 24 hours it
is marked `failed` and `notification.failed` is recorded, so a dropped alert is
never silent. A failure the provider marks permanent (`kitnotify.Permanent`: an
HTTP 4xx other than 408 and 429, an SMTP 5xx reply) is given up the same way on
the spot, since retrying cannot change the answer. Delivery is at-least-once: a send that succeeded but could not be
marked delivered is repeated after the lease.

**Delivery is ordered per channel, for a bounded time.** A pending delivery
holds back the ones queued after it on its channel (and only on its channel)
until it is delivered or given up, so a receiver sees events in the order they
happened, `heartbeat.missed` before its `heartbeat.recovered`, even after a
short outage. The hold is bounded so that one delivery cannot silence the
channel: once the holding delivery was first attempted (`first_attempt_at`)
more than `Worker.Hold` ago (15 minutes, `TEND_DELIVERY_HOLD_MINUTES`), the
claim takes the deliveries behind it while it keeps retrying until its own
give-up, and ordering resumes among those. A destination's permanent refusal
is given up at once and holds nothing. A hold of 0 turns ordering off.

**The runner's `EventSink`** (`Runner.EventSink func(context.Context,
core.Event)`) is the seam through which the runner signals that a terminal event
was recorded. It is a plain `func` over `core.Event` (not a `notify` type)
precisely so `jobs` never imports `notify`, keeping the graph acyclic. In
`serve`, the sink, and the same closure handed to the HTTP server and the
watcher, only calls `Worker.Nudge()` to wake the worker for a prompt drain.
Nothing is sent inline, so a slow or failing destination never blocks a runner
worker, a ping response, or the watcher. Under `--no-master-key` the worker is
not started (channel config cannot be decrypted) and the closure is nil; events
are still recorded and their deliveries stay queued until `serve` runs with a
key.
`tend run` drains the queue once inline after its run, so a manual run notifies
even when no server is running.

---

## 5. Config-as-code

`internal/configfile` makes resource **definitions** declarative.

- **YAML is the source of truth** for jobs, notification channels, notification
  rules, and heartbeats. `Parse` validates the file into typed specs (naming the
  offending entry on any error).
- **`sync` reconciles one-way into the DB.** `Reconcile` applies the config in
  dependency order (jobs → channels → rules → heartbeats). Every section is an
  idempotent **upsert** (by name), so a re-run converges. Reconcile is *not*
  atomic across sections, but because every operation is an idempotent upsert in
  dependency order, a fixed re-run reaches the desired state with no orphaned
  rows.
- **Declarative removal = disable-on-absence.** Jobs present in the DB but absent
  from the config are **disabled** (not hard-deleted) on sync. This is the prune
  behavior and it is gated: `-prune=false` leaves absent jobs untouched.
  Channels, rules, and heartbeats absent from config are left in place in the
  current version.
- **Secret refs are resolved, not stored in YAML.** A value of the form
  `{{ secret.NAME }}` (channel config, or a job's `env`) is kept verbatim by
  `Parse` and resolved at the appropriate time: channel config secrets are
  resolved during `Reconcile`; a job's `env` secrets are resolved at **run time**
  by the runner (so secret plaintext is never persisted in the job row).

---

## 6. Security model

The security guarantees are enforced structurally where possible; the point is
that secret material has nowhere to go, not that every call site remembers to be
careful.

- **Passwords: argon2id (PHC).** `auth.HashPassword` produces a standard PHC
  string (`$argon2id$v=19$m=…,t=…,p=…$salt$hash`) with a fresh 16-byte random
  salt. The parameters travel inside the stored hash, so they can change later
  without invalidating existing hashes. `VerifyPassword` parses defensively and
  compares in constant time. (The login path also runs a dummy verification on
  the unknown-email branch to equalize timing and prevent account enumeration.)
- **Sessions: stateless, signed.** The session cookie is an HMAC-SHA256-signed
  payload (`user_id || org_id || exp`, base64url, with the MAC appended); no
  server-side session store. `Decode` rejects tampered or expired tokens.
- **One master key underpins encryption *and* session signing.** The session
  signing key is **HKDF-SHA256-derived from the base64 master key** (see
  `deriveSessionKey` in `internal/cli`), with a fixed info label
  (`tend-session-v1`) and no salt (deterministic), so restarts don't invalidate
  outstanding sessions. The same master key feeds the secrets `Box`. One secret,
  domain-separated by HKDF, underpins both.
- **Secrets: AES-256-GCM `Box`.** `secrets.Box` (`kit/secrets`) encrypts with
  AES-256-GCM and a fresh random nonce per call (prepended to the ciphertext); the
  master key is a base64-encoded 32-byte value. Channel config and stored secret
  values are encrypted at rest.
- **API tokens: hashed at rest.** A token is `tend_` + base64url(32 random
  bytes), shown to the user exactly once. Only `HashToken` =
  `hex(sha256(token))` is persisted (plain SHA-256 is sufficient because tokens
  carry full random entropy). `AuthenticateToken` matches strictly on the full
  hash; a miss is `ErrNotFound`, never a partial match. `ListTokens` does not even
  SELECT the hash column.
- **Leak-free-by-construction DTOs.** The REST API and dashboard never serialize
  domain types. Every wire shape is a purpose-built DTO with explicit `json` tags
  that has **no field** for secret material: no `token`, no `*_hash`, no
  ciphertext. A heartbeat DTO has no `Token` field; a channel DTO has no `config`
  field; the secret DTO carries only name + created-at. You cannot leak a secret
  through these paths because there is structurally nowhere to put it. The store
  reinforces this: `ListSecrets` selects only `name, created_at`, never the
  ciphertext column.
- **CSRF on cookie-auth mutations.** `requireAuth` enforces a session-bound CSRF
  token (HMAC over the session identity) on cookie-authenticated unsafe methods.
  Bearer-token (API) requests carry no ambient cookie and are CSRF-exempt by
  design.
- **Org-scoping on every query.** Every tenant-scoped store method takes an
  `orgID`, and handlers scope every call to `Principal.OrgID` resolved by
  `requireAuth`. A resource id belonging to another org is simply not found.
- **Rate limiting on the unauthenticated endpoints.** `POST /login` (burst 5,
  refill 1 per second) passes through an in-memory token bucket keyed by client
  address and endpoint (`kit/httpx`'s `RateLimiter`). `/ping/...`
  has two buckets, checked before the database is touched: unknown tokens (and
  unknown ping keys or slugs) are
  counted per client address (burst 30, refill 1 per second), and once an
  address has spent that budget every ping from it is refused, whatever token
  it sends next, so rotating tokens does not evade it; pings for a real
  heartbeat are counted per token (burst 10, refill 1 per second) and never
  per address, so a host running many heartbeats on the same minute is never
  throttled for it. Requests over a limit get `429` with `Retry-After`; ping
  clients are expected to retry. The client address is the TCP
  peer unless `TEND_TRUST_PROXY` is set, in which case the left-most
  `X-Forwarded-For` entry is used; that is only sound behind a proxy that
  overwrites the header. The entry must parse as an IP address and is keyed in
  canonical form; anything else falls back to the TCP peer, so a header value
  cannot become an arbitrary bucket key. Buckets live in the server process;
  idle ones are evicted by a sweep that runs at most every 10 minutes.
- **Security headers on every response.** `httpx.SecurityHeaders` wraps the whole mux
  and sets `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
  `Referrer-Policy: no-referrer`, and a `Content-Security-Policy` built on
  `default-src 'self'` and `frame-ancestors 'none'`, with no inline scripts or
  styles allowed. Templates therefore must not use inline event handlers,
  inline `<script>` blocks, or `style` attributes; row navigation lives in
  `static/row-nav.js` for that reason, and the base template turns off the
  indicator stylesheet htmx would otherwise inject. HSTS is deliberately not
  sent, because `serve` speaks plain HTTP and TLS is terminated at a reverse
  proxy.
- **URLs are kept out of error text.** A failed outbound request surfaces as a
  `*url.Error` that quotes the full URL. `kit/urlredact` reduces it to the
  host and the underlying cause; the webhook, Slack, Discord, Telegram, ntfy
  and Pushover providers and the HTTP job executor all go through it, so a token embedded in
  a URL reaches neither the logs nor `job_runs.output`. Two causes would quote
  a few characters of the URL themselves, a bad percent-escape and a character
  that is not allowed in a host name (`url.EscapeError`,
  `url.InvalidHostError`); those are replaced by a fixed phrase. So is any
  other reason for which `net/url` rejects the URL, unless it is one of its
  fixed phrases ("missing protocol scheme"): `invalid port ":..." after host`
  quotes everything after the colon, which in a URL that has lost its host is
  the token, and is reported as "invalid URL".

`requireAuth` resolves a `Principal` from a session cookie **or** an
`Authorization: Bearer` token, and **fails closed**: any error decoding the
cookie, loading the user/membership, or matching the token leaves the request
unauthenticated (a 401 JSON body for `/api/...`, a 302 to `/login` otherwise),
without revealing which step failed.

---

## 7. Capability asymmetries (by design)

These asymmetries are intentional. They exist to prevent **drift** between the
declared config and runtime state, and to keep secret-write paths off the network.

- **The HTTP API is read + exactly three mutations.** It serves read-only `GET`
  endpoints plus exactly three actions: **run-now** (`POST
  /api/jobs/{id}/run`), **enable** (`POST /api/jobs/{id}/enable`), and **disable**
  (`POST /api/jobs/{id}/disable`). It **never** creates, edits, or deletes
  resource definitions. Definitions live in the CLI and config so the API can't
  drift the running state away from what's declared.
- **`job rm` is a CLI-only hard delete.** The imperative CLI hard-deletes a job
  and its dependents (transactionally, see `DeleteJob`). The declarative model
  removes a job differently: by **absence** from the config → disable-on-sync
  (prune), *not* a hard delete. The two removal models are deliberately
  different.
- **Secrets have a CLI-only write path.** `tend secret set <key>` reads the value
  from **stdin** (never argv, to avoid process-list leakage), encrypts it, and
  stores the ciphertext. There is **no** secret-write path through YAML and
  **no** secret-write path through the API. YAML and jobs only *reference*
  secrets by name (`{{ secret.NAME }}`).
- **Type-specific fields are inert on the wrong type.** A job's `env` takes effect
  for the commands Tend runs (a **shell** job's command, an **expiry** job's
  `expiry_command`, a **drill**'s steps); `http_body` applies to **http** jobs
  only. Both fields are accepted on any job type but are simply ignored on the
  type they don't apply to. A probe's or drill's settings (`jobs.Spec`) are
  narrower still: `tend job add` and `tend sync` keep only the fields of the
  job's own type, apply its defaults, and refuse settings `ValidateSpec`
  rejects before anything is written; the executor validates them again
  before each run, so a row written by other means fails its run with the
  same message instead of misbehaving.

---

## 8. Operational safeguards

These limits are constants in the code, not configuration.

- **No implicit database.** `config.Load` leaves the DSN empty when `TEND_DB` is
  unset, and `cli.Run` then refuses every command that would open the store.
  `version`, top-level help (`help`, `-h`, `--help`), `serve -h` and `exec`
  are answered before that check, so they need no configuration and never
  create a database file or start the daemon. Every other subcommand parses its flags
  after the store is open, so its `-h` still needs `TEND_DB`.
- **Bounded captured output.** The executor keeps at most `maxOutputBytes`
  (1 MiB) per attempt: shell jobs write through a `cappedBuffer` that discards
  the excess without ever returning an error (so a chatty child is not killed by
  a full pipe), and HTTP jobs read the body through an `io.LimitReader`. A
  truncation marker is appended, after backing the cut off to a character
  boundary so truncation never produces invalid UTF-8; status and exit code are
  unaffected. A run stores the output of its last attempt. Replacing invalid
  bytes and redacting a short secret can make the text longer than the bytes
  that were captured, so the runner applies the cap once more to what it is
  about to store: the stored output, the `HTTP <status>` line of an HTTP job
  included, never exceeds 1 MiB plus the marker. That second cut comes after
  redaction. The capture cap does not: it falls on the raw bytes, so the first
  part of a secret that straddles the end of a capture cut at 1 MiB matches
  nothing and is stored. This is a known limitation.
- **Retention sweep.** `serve` prunes at startup and every 24 hours: by default
  events older than 30 days, terminal job runs older than 90 days, finalized
  deliveries older than 7 days, heartbeat ping log entries older than 30 days
  (`config.LoadRetention`, from `TEND_RETENTION_EVENTS_DAYS`,
  `TEND_RETENTION_RUNS_DAYS`, `TEND_RETENTION_DELIVERIES_DAYS` and
  `TEND_RETENTION_PINGS_DAYS`; `0` keeps that history forever and an
  invalid value stops `serve` from starting). Each heartbeat also keeps only
  its newest `store.MaxPingsPerHeartbeat` (1000) pings, and of those only as
  many as fit in `store.MaxPingLogBytes` (10 MB) of bodies, trimmed in the ping's
  own transaction, so a runaway client cannot fill the disk between sweeps. When the sweep deleted rows it
  calls `ReclaimSpace`: on SQLite, once at least 8 MiB and a quarter of the file
  is free, the first call switches the file to incremental auto-vacuum with one
  `VACUUM` (only for a file of at most 64 MiB, since the store's single
  connection is held for the whole rewrite; a larger file is reported once per
  process, for an offline `PRAGMA auto_vacuum = INCREMENTAL; VACUUM;`). Later
  calls free pages in `incremental_vacuum` steps of 512 pages, each its own
  statement through the pool, so queued pings and runs get the connection
  between two steps, and stop after about 2 seconds; the next sweep carries
  on. On Postgres it does nothing; autovacuum makes the space reusable. A
  failed sweep is logged and retried on the next tick.
- **HTTP server timeouts.** `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`,
  and `IdleTimeout` are all 10 seconds (`httpx.NewServer`, called by `serve`).
  Every handler is quick and the largest request body is a ping's (read up to
  its body limit, at most 100 kB, the rest discarded), so a
  client that stalls while sending its headers or body, while reading the
  response, or between requests cannot hold a connection open.
- **Probes and drills.** Every command a probe or a drill runs goes through
  `shellCommand`, the shell job's own mechanics (`sh -c`, the job's env, its
  own process group killed at the timeout); nothing is ever spliced into a
  command, and what a drill's steps need (`TEND_SCRATCH`, `TEND_DRILL_ID`)
  reaches them through the environment. An `expiry_command`'s stdout is read
  as one date and nothing else (at most 4 KiB); its stderr is kept (16 KiB).
  A `tls` probe completes the handshake without verifying and then verifies
  the chain itself (`checkCertificate`), against the system roots plus
  `ca_file`, so that every problem is reported, not the first; it sends
  nothing over the encrypted connection, and refuses a STARTTLS server that
  sends data before the handshake. A `domain` probe reads at most 1 MiB of
  RDAP answer and 4 MiB of IANA bootstrap file, which is cached in memory for
  a day per process (`rdapBootstraps`) and used past that when IANA cannot be
  reached. A drill keeps the first and last 32 KiB of each step's output
  (`headTail`); its cleanup runs however the restore ended, detached from the
  attempt's context and bounded by `drillCleanupTimeout` (one minute), and its
  scratch directory is removed after it, made writable first when a restore
  left read-only directories. Because that cleanup can run past the attempt's
  timeout, `maxRunDuration` allows a drill's attempts that much more, so the
  reaper does not fail a drill that is cleaning up.
- **Cron validation at the edges.** `jobs.ValidateCron` uses the same parser as
  `Job.NextRun`. `tend job add` and `configfile.Parse` call it before anything is
  written, so an expression that would never fire is rejected instead of stored.
- **Deployment files.** `deploy/tend.service` is the hardened system-wide unit
  and `deploy/tend-user.service` a user-level unit. `deploy/deploy-tend.sh`
  builds the binary, stops the service, keeps the previous binary, copies the
  new one, starts it, and rolls back if `/healthz` does not answer. The script
  and the user unit carry the host name, paths, and addresses of the
  maintainers' own installation and are meant to be adapted, not run as is.

---

## Where to start reading

- The persistence seam and parity conventions: `internal/store/store.go`, then
  `internal/store/sqlite.go` and `internal/store/postgres.go`.
- The execution engine: `internal/jobs/runner.go` and `internal/jobs/executor.go`;
  the probes and drills in `internal/jobs/probe_*.go` and `internal/jobs/drill.go`,
  their settings in `internal/jobs/spec.go`.
- The notification path: `internal/notify/worker.go` (the queue and send step
  kit's worker drives; its retries are in `kit/notify`),
  `internal/notify/message.go` (what is alertable, how messages are rendered),
  and `enqueueDeliveriesTx` in `internal/store/sqlite.go` (the enqueue side).
- The auth/security surface: `internal/httpserver/auth.go` (over the primitives
  in `kit/auth`); DTOs in `internal/httpserver/api.go`.
- Config-as-code: `internal/configfile/configfile.go`.
- The wiring of `serve`: `internal/cli/cli.go`.
