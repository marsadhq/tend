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
  notification dispatcher**:
  - the **runner** (scheduler tick + worker goroutines that claim and execute
    runs),
  - the **HTTP server** (heartbeat ping endpoint, `/healthz`, the read + action
    REST API, and the htmx dashboard),
  - the **heartbeat watcher** (periodic scan for missed dead-man's-switch pings).

  All three share the injected `clock.Clock` and a single
  `func(context.Context, core.Event)` dispatch closure, so behavior is consistent
  and testable (a `clock.FakeClock` drives time in tests).

Other subcommands (`sync`, `job`, `run`, `logs`, `secret`, `channel`, `rule`,
`heartbeat`, `user`, `token`, `version`) are one-shot CLI operations dispatched
from `internal/cli`.

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
| `internal/jobs` | Job/Run domain types, scheduling (`Job.NextRun`), the `Executor` (shell + HTTP), and the `Runner` loop (scheduler + workers + secret resolution + output redaction). |
| `internal/secrets` | The AES-256-GCM `Box` used to encrypt/decrypt secret values and channel config. |
| `internal/auth` | Cryptographic identity primitives: argon2id passwords, API tokens, signed session cookies, CSRF; the `Principal`, `User`, `APIToken`, `Membership` types. |
| `internal/notify` | The notification domain: channel types, the `Provider` abstraction (webhook/Slack/Discord/SMTP), rules, and the `Dispatcher`. |
| `internal/heartbeat` | The `Heartbeat` domain type and the `Watcher` that marks missed heartbeats down. |
| `internal/configfile` | YAML config-as-code: `Parse` (file → structs) and `Reconcile` (structs → DB, one-way). |
| `internal/httpserver` | The HTTP surface: `requireAuth` middleware, the read + action REST API with leak-free DTOs, the htmx dashboard, login/logout, and the heartbeat ping/health endpoints. |
| `internal/config` | Environment-driven process configuration (driver, DSN, master key, cookie-secure flag, etc.). |
| `internal/cli` | Subcommand dispatch and the `serve` wiring that ties the runner, HTTP server, watcher, and dispatcher together. |

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
- `notify.DispatchStore` / `notify.ChannelStore`: what the dispatcher and channel
  helpers need.
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

The claim path differs subtly by engine: SQLite serializes writes at the pool
(`SetMaxOpenConns(1)`) and uses a single atomic `UPDATE … RETURNING` to claim the
oldest pending run; Postgres allows real connection concurrency so `FOR UPDATE
SKIP LOCKED`-style claiming is meaningful. Both guarantee no two workers claim the
same run.

Crash recovery is at-least-once: `RequeueOrphanedRuns` resets any `running` rows
back to `pending` at startup (the single-instance runner has no peers, so a
`running` row found at boot was orphaned by a crash).

---

## 4. Event pipeline and the `EventSink` seam

`core.Event` is the generic record carried by the event pipeline: `{ID, OrgID,
Type, Source, Payload (JSON), DedupKey, CreatedAt}`. Events are the spine that ties
the runner, watcher, and notifier together.

Event types currently emitted:

- **Runs:** `run.started` (best-effort), `run.succeeded`, `run.failed`. Timeouts
  surface as `run.failed` with the precise status in the payload, so the
  `run.*` type vocabulary stays small.
- **Heartbeats:** `heartbeat.missed`, `heartbeat.recovered`.
- **Notifications:** `notification.failed` (emitted when delivery is exhausted).

**Terminal run events are written atomically with the run.** The runner records
the terminal run state and appends the terminal event in one transaction via
`FinishRunAndEmit`. This closes the lost-event gap where `FinishRun` could commit
but a separate `EmitEvent` then fail. (`run.started` is explicitly *not* terminal
and is best-effort; a lost start event is non-critical because the terminal
event is guaranteed.)

**The runner's `EventSink`** (`Runner.EventSink func(context.Context,
core.Event)`) is the seam through which terminal events reach the notifier. After
a terminal event is durably recorded, the runner fires the sink. It is a plain
`func` over `core.Event` (not a `notify` type) precisely so `jobs` never imports
`notify`, keeping the graph acyclic. In `serve`, the sink is the dispatcher's
`DispatchForEvent`. The runner fires on **every** terminal event (successes
included) and lets the dispatcher decide what is alertable.

**The dispatcher's loop-guard.** `notify.Dispatcher` only acts on an `alertable`
set (`run.failed`, `heartbeat.missed`, `heartbeat.recovered`). Non-alert events,
including all `notification.*` events, are dropped *before any store query*. This
is the loop guard: a delivery failure emits `notification.failed`, which can never
feed back in and start a notification storm. It also means the runner can fire on
`run.succeeded` harmlessly; the filter drops it.

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

## Where to start reading

- The persistence seam and parity conventions: `internal/store/store.go`, then
  `internal/store/sqlite.go` and `internal/store/postgres.go`.
- The execution engine: `internal/jobs/runner.go` and `internal/jobs/executor.go`.
- The notification path: `internal/notify/dispatcher.go`.
- The auth/security surface: `internal/auth/auth.go` and
  `internal/httpserver/auth.go`; DTOs in `internal/httpserver/api.go`.
- Config-as-code: `internal/configfile/configfile.go`.
- The wiring of `serve`: `internal/cli/cli.go`.
