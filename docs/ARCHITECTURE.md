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
  - the **HTTP server** (heartbeat ping endpoint, `/healthz`, the read + action
    REST API, and the htmx dashboard),
  - the **heartbeat watcher** (periodic scan for missed dead-man's-switch pings),
  - the **delivery worker** (drains the durable notification queue; started
    only when a master key is configured),
  - the **retention sweep** (prunes old events, job runs, and deliveries at
    startup and then daily).

  The runner, HTTP server, and watcher share the injected `clock.Clock` and a
  single `func(context.Context, core.Event)` closure that nudges the delivery
  worker, so behavior is consistent and testable (a `clock.FakeClock` drives
  time in tests).

Other subcommands (`sync`, `job`, `run`, `logs`, `secret`, `channel`, `rule`,
`heartbeat`, `doctor`, `user`, `token`, `version`) are one-shot CLI operations
dispatched from `internal/cli`.

---

## 2. Package layout

All application code lives under `internal/` (so nothing is importable as a public
API). `cmd/tend/main.go` is the entry point: it loads config, installs a
SIGINT/SIGTERM-cancelled context, and calls `cli.Run`.

| Package | Responsibility |
| --- | --- |
| `internal/core` | Cross-cutting domain types shared by everything: `Org` (tenant) and the generic `Event` record (the event-pipeline spine). |
| `internal/clock` | The `Clock` interface, `RealClock`, and a concurrency-safe `FakeClock` for tests. |
| `internal/store` | The `Store` interface (single persistence seam) plus the SQLite and Postgres implementations and embedded SQL migrations. |
| `internal/jobs` | Job/Run domain types, scheduling (`Job.NextRun`, `ValidateCron`), the `Executor` (shell + HTTP, with capped output), and the `Runner` loop (scheduler + workers + stale-run reaper + secret resolution + output redaction). |
| `internal/secrets` | The AES-256-GCM `Box` used to encrypt/decrypt secret values and channel config. |
| `internal/auth` | Cryptographic identity primitives: argon2id passwords, API tokens, signed session cookies, CSRF; the `Principal`, `User`, `APIToken`, `Membership` types. |
| `internal/notify` | The notification domain: channel types, the `Provider` abstraction (webhook/Slack/Discord/SMTP/Telegram), rules, the `Alertable` filter, and the `Worker` that drains the durable delivery queue. |
| `internal/heartbeat` | The `Heartbeat` domain type and the `Watcher` that marks missed heartbeats down. |
| `internal/configfile` | YAML config-as-code: `Parse` (file → structs) and `Reconcile` (structs → DB, one-way). |
| `internal/httpserver` | The HTTP surface: `requireAuth` middleware, the read + action REST API with leak-free DTOs, the htmx dashboard, login/logout, the heartbeat ping/health endpoints, the security-headers middleware, and the rate limiter for `/login` and `/ping`. |
| `internal/urlredact` | Stdlib-only helpers that reduce a URL to its host and a request error to its cause, shared by `notify` and `jobs` so secret-bearing URLs stay out of logs and run output. |
| `internal/config` | Environment-driven process configuration (driver, DSN, master key). `TEND_DB` has no default; an unset value leaves the DSN empty and `cli.Run` refuses to open a database. |
| `internal/cli` | Subcommand dispatch and the `serve` wiring that ties the runner, HTTP server, watcher, delivery worker, and retention sweep together. |

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
  references.

The claim path differs subtly by engine: SQLite serializes writes at the pool
(`SetMaxOpenConns(1)`) and uses a single atomic `UPDATE … RETURNING` to claim the
oldest pending run; Postgres allows real connection concurrency so `FOR UPDATE
SKIP LOCKED`-style claiming is meaningful. Both guarantee no two workers claim the
same run.

Crash recovery is at-least-once: `RequeueOrphanedRuns` resets any `running` rows
back to `pending` at startup (the single-instance runner has no peers, so a
`running` row found at boot was orphaned by a crash).

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
Type, Source, Payload (JSON), DedupKey, CreatedAt}`. Events are the spine that ties
the runner, watcher, and notifier together.

Event types currently emitted:

- **Runs:** `run.started` (best-effort), `run.succeeded`, `run.failed`. Timeouts
  surface as `run.failed` with the precise status in the payload, so the
  `run.*` type vocabulary stays small.
- **Heartbeats:** `heartbeat.missed`, `heartbeat.recovered`. A heartbeat that
  was never pinged is armed from its creation time, so `heartbeat.missed` also
  covers a first ping that never arrives.
- **Notifications:** `notification.failed` (emitted when delivery is exhausted).

**Terminal run events are written atomically with the run.** The runner records
the terminal run state and appends the terminal event in one transaction via
`FinishRunAndEmit`. This closes the lost-event gap where `FinishRun` could commit
but a separate `EmitEvent` then fail. (`run.started` is explicitly *not* terminal
and is best-effort; a lost start event is non-critical because the terminal
event is guaranteed.)

**Notifications are a durable queue.** Every event insert (`EmitEvent`,
`FinishRunAndEmit`, `ReapStaleRun`) also inserts, in the same transaction, one
`pending` row in the `deliveries` table for each channel that has an enabled
rule matching the event: org-wide rules plus rules scoped to the event's job.
Because the delivery commits atomically with the event, neither a crash nor a
slow destination can lose a matched notification. Overlapping rules for the
same channel produce a single delivery.

**The loop guard lives at enqueue time.** Only the `alertable` set
(`run.failed`, `heartbeat.missed`, `heartbeat.recovered`; see
`notify.Alertable`) ever enqueues deliveries. Every other event type, including
all `notification.*` events, enqueues nothing. A delivery that is given up on
emits `notification.failed`, which therefore can never feed back in and start a
notification storm, and emitters can record `run.succeeded` freely.

**`notify.Worker` drains the queue.** It claims due rows (`ClaimDueDeliveries`
increments the attempt count and pushes `next_attempt_at` out by a one-minute
lease, so a claim held by a crashed worker simply becomes due again), decrypts
the channel config, builds the provider, and sends. Success marks the row
`delivered`. Failure reschedules it with exponential backoff (1 second,
doubling, capped at 5 minutes). Once a delivery has been failing for 24 hours it
is marked `failed` and `notification.failed` is emitted, so a dropped alert is
never silent. Delivery is at-least-once: a send that succeeded but could not be
marked delivered is repeated after the lease.

**The runner's `EventSink`** (`Runner.EventSink func(context.Context,
core.Event)`) is the seam through which the runner signals that a terminal event
was recorded. It is a plain `func` over `core.Event` (not a `notify` type)
precisely so `jobs` never imports `notify`, keeping the graph acyclic. In
`serve`, the sink, and the same closure handed to the HTTP server and the
watcher, only calls `Worker.Nudge()` to wake the worker for a prompt drain.
Nothing is sent inline, so a slow or failing destination never blocks a runner
worker, a ping response, or the watcher. Without a master key the worker is not
started (channel config cannot be decrypted) and the closure is nil; events are
still recorded and their deliveries stay queued until `serve` runs with a key.
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
- **Secrets: AES-256-GCM `Box`.** `secrets.Box` (`internal/secrets`) encrypts with
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
  refill 1 per second) and `/ping/{token}` (burst 20, refill 10 per second) pass
  through an in-memory token bucket keyed by client address and endpoint
  (`internal/httpserver/ratelimit.go`); requests over the limit get `429` with
  `Retry-After`. The ping limiter keys on the address rather than the token on
  purpose, so rotating tokens does not evade it. The client address is the TCP
  peer unless `TEND_TRUST_PROXY` is set, in which case the left-most
  `X-Forwarded-For` entry is used; that is only sound behind a proxy that
  overwrites the header. The entry must parse as an IP address and is keyed in
  canonical form; anything else falls back to the TCP peer, so a header value
  cannot become an arbitrary bucket key. Buckets live in the server process;
  idle ones are evicted by a sweep that runs at most every 10 minutes.
- **Security headers on every response.** `securityHeaders` wraps the whole mux
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
  `*url.Error` that quotes the full URL. `internal/urlredact` reduces it to the
  host and the underlying cause; the webhook, Slack, Discord, and Telegram
  providers and the HTTP job executor all go through it, so a token embedded in
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
  for **shell** jobs only; `http_body` applies to **http** jobs only. Both fields
  are accepted on any job type but are simply ignored on the type they don't
  apply to.

---

## 8. Operational safeguards

These limits are constants in the code, not configuration.

- **No implicit database.** `config.Load` leaves the DSN empty when `TEND_DB` is
  unset, and `cli.Run` then refuses every command that would open the store.
  `version`, top-level help (`help`, `-h`, `--help`), and `serve -h` are
  answered before that check, so they need no configuration and never create a
  database file or start the daemon. Every other subcommand parses its flags
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
- **Retention sweep.** `serve` prunes at startup and every 24 hours: events and
  terminal job runs older than 30 days, finalized deliveries older than 7 days
  (the constants sit next to `cmdServe`). A failed sweep is logged and retried
  on the next tick.
- **HTTP server timeouts.** `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`,
  and `IdleTimeout` are all 10 seconds (`newHTTPServer` in `internal/cli`).
  Every handler is quick and the largest request body is a login form, so a
  client that stalls while sending its headers or body, while reading the
  response, or between requests cannot hold a connection open.
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
- The execution engine: `internal/jobs/runner.go` and `internal/jobs/executor.go`.
- The notification path: `internal/notify/worker.go` (delivery and retries),
  `internal/notify/message.go` (what is alertable, how messages are rendered),
  and `enqueueDeliveriesTx` in `internal/store/sqlite.go` (the enqueue side).
- The auth/security surface: `internal/auth/auth.go` and
  `internal/httpserver/auth.go`; DTOs in `internal/httpserver/api.go`.
- Config-as-code: `internal/configfile/configfile.go`.
- The wiring of `serve`: `internal/cli/cli.go`.
