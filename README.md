<p align="center">
  <img src="docs/assets/lockup.svg" alt="tend job runner" height="88">
</p>

<p align="center">
  <strong>Cron jobs, run monitoring, alerting, and dead-man's-switch heartbeats in one self-hostable static binary.</strong>
</p>

<p align="center">
  <a href="https://github.com/marsadhq/tend/actions/workflows/ci.yml"><img src="https://github.com/marsadhq/tend/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/marsadhq/tend/releases"><img src="https://img.shields.io/github/v/release/marsadhq/tend" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-blue.svg" alt="License: AGPL-3.0"></a>
  <a href="https://goreportcard.com/report/github.com/marsadhq/tend"><img src="https://goreportcard.com/badge/github.com/marsadhq/tend" alt="Go Report Card"></a>
</p>

Tend is a self-hosted job runner with a web dashboard. It runs shell commands and HTTP calls on cron or interval schedules, records every run (history, captured output, exit codes), watches for missed heartbeats, and fires webhook/Slack/Discord/SMTP alerts.

![Tend jobs dashboard](docs/assets/dashboard-jobs.png)

---

## Why Tend

Most teams stitch this together from three or four moving parts: system cron to schedule, a hosted service like Healthchecks or Cronitor to watch for misses, and a separate notifier to page someone. Tend folds those concerns into a single program you run yourself:

- **Scheduling**: shell or HTTP jobs on cron expressions, fixed intervals, or a one-off `run_at` time.
- **Run monitoring**: every execution is recorded with its status, exit code, timing, and captured output (up to 1 MiB per attempt), browsable in the dashboard or over the HTTP API.
- **Alerting**: route `run.failed`, `heartbeat.missed`, and other events to webhook, Slack, Discord, SMTP, or Telegram channels, globally or scoped to a single job.
- **Dead-man's-switch heartbeats**: give Tend a ping URL for any external job; if a ping doesn't arrive within its period plus grace, Tend alerts.

It's a **single static, CGO-free binary**. SQLite is the default store (no database to stand up), with an optional Postgres backend for a managed or external database. **No Redis, no external dependencies.** Configure it imperatively with the CLI or declaratively with a YAML file you keep in version control.

### Screenshots

| | |
|---|---|
| ![Captured run output](docs/assets/run-output.png) | **Run output**: a failed run with its captured stdout/stderr and exit code. |
| ![Heartbeat statuses](docs/assets/heartbeats.png) | **Heartbeats**: dead-man's-switch monitors showing up / down / new status. |
| ![Activity and alert feed](docs/assets/events.png) | **Events**: the activity and alert feed across all jobs and heartbeats. |
| ![Job detail](docs/assets/job-detail.png) | **Job detail**: schedule, recent runs, and configuration for one job. |

---

## Install

**One-line install** (Linux, amd64/arm64). Downloads the latest release and verifies its sha256 against the release `checksums.txt` before installing; it never installs an unverified binary:

```sh
curl -fsSL https://raw.githubusercontent.com/marsadhq/tend/master/install.sh | sh
```

Override the install location or pin a version with environment variables:

```sh
PREFIX="$HOME/.local" curl -fsSL https://raw.githubusercontent.com/marsadhq/tend/master/install.sh | sh   # binary at $PREFIX/bin/tend
VERSION=0.1.0          curl -fsSL https://raw.githubusercontent.com/marsadhq/tend/master/install.sh | sh   # pin a specific release
```

**Docker:**

```sh
docker run -d --name tend -p 8080:8080 \
  -v tenddata:/data \
  -e TEND_MASTER_KEY="$(head -c 32 /dev/urandom | base64)" \
  ghcr.io/marsadhq/tend:latest
```

---

## 60-second quickstart

### Docker

```sh
# 1. Generate a master key (do this once; save it; never change it)
export TEND_MASTER_KEY="$(head -c 32 /dev/urandom | base64)"

# 2. Start the server
docker volume create tenddata
docker run -d --name tend \
  -p 8080:8080 \
  -v tenddata:/data \
  -e TEND_MASTER_KEY="$TEND_MASTER_KEY" \
  ghcr.io/marsadhq/tend:latest

# 3. Create the first user (admin); password is read from stdin
printf 'yourpassword' | docker run --rm -i \
  -v tenddata:/data \
  -e TEND_DB=/data/tend.db \
  -e TEND_MASTER_KEY="$TEND_MASTER_KEY" \
  ghcr.io/marsadhq/tend:latest \
  user add -email you@example.com

# 4. Open http://localhost:8080/login and sign in
```

The image's `ENTRYPOINT` is `/tend` and the default `CMD` is `serve`, so passing a subcommand (e.g. `user add`) replaces `serve`. The data volume must match what `serve` uses (`/data/tend.db` by default). If the server container is already running, you can instead run:

```sh
printf 'yourpassword' | docker exec -i tend /tend user add -email you@example.com
```

### Binary

```sh
# 1. Install (see above), or build from source: `make build` produces bin/tend
#    (build-from-source: either add ./bin to PATH, prefix the commands below with
#     ./bin/ (e.g. ./bin/tend serve), or run `sudo install bin/tend /usr/local/bin/tend`
#     so the bare `tend` commands below work verbatim)

# 2. Generate a master key (once; stable across restarts)
export TEND_MASTER_KEY="$(head -c 32 /dev/urandom | base64)"
export TEND_DB=/var/lib/tend/tend.db   # pick any writable path

# 3. Start the server (stays in foreground; use systemd or a process supervisor)
tend serve

# 4. In a second terminal, create the first user
printf 'yourpassword' | TEND_DB=/var/lib/tend/tend.db \
  TEND_MASTER_KEY="$TEND_MASTER_KEY" \
  tend user add -email you@example.com

# 5. Open http://localhost:8080/login and sign in
```

For unattended operation see [`deploy/tend.service`](deploy/tend.service) (a hardened system-wide systemd unit), [`deploy/tend-user.service`](deploy/tend-user.service) (a user-level unit for a single-operator install without root), and [`deploy/docker-compose.yml`](deploy/docker-compose.yml).

[`deploy/deploy-tend.sh`](deploy/deploy-tend.sh) ships a build to a host running the user unit over ssh. It refuses a dirty working tree or a `HEAD` that is not `origin/master` (override with `--allow-dirty` / `--allow-any-commit`), builds and uploads the new binary while the old one keeps serving, snapshots the SQLite database with the backup API and checks the copy, and only then stops the service, swaps the binary (keeping the previous one), starts it again, and rolls back if `/healthz` does not answer. From the stop onwards every way out of the script, a failure or Ctrl-C included, starts the service again. The host, health URL, database path and remote directories come from the environment or from `deploy/deploy.env` (not tracked; copy [`deploy/deploy.env.example`](deploy/deploy.env.example)), so nothing host-specific is in the repository.

---

# Reference

## Environment variables

| Variable             | Default                 | Description |
|----------------------|-------------------------|-------------|
| `TEND_DB`            | *(required)*            | SQLite file path **or** a `postgres://` / `postgresql://` URL. There is no default: every command that opens the database refuses to run while it is unset (`tend version`, `tend help`, and `tend serve -h` are the exceptions). Use an absolute path for SQLite. The Docker image sets it to `/data/tend.db` and the shipped system unit to `/var/lib/tend/tend.db`. |
| `TEND_MASTER_KEY`    | *(required by `serve`)* | Base64-encoded 32-byte key. Required for alerting, the dashboard, session signing, and secrets encryption. Generate once with `head -c 32 /dev/urandom \| base64`. **Must be stable** across restarts; changing it invalidates all sessions and makes all stored secrets unreadable. |
| `TEND_ADDR`          | `:8080`                 | TCP listen address for the HTTP server. |
| `TEND_BASE_URL`      | `http://localhost:8080` | Externally-reachable base URL. Used when printing heartbeat ping URLs. Set this when Tend is behind a reverse proxy. |
| `TEND_COOKIE_SECURE` | `false`                 | Set to `1`, `true`, `yes`, or `on` to mark session cookies as `Secure` (HTTPS-only). Leave `false` when TLS is terminated at a reverse proxy. |
| `TEND_RETENTION_EVENTS_DAYS` / `TEND_RETENTION_RUNS_DAYS` / `TEND_RETENTION_DELIVERIES_DAYS` | `30` / `90` / `7` | Days of events, finished job runs, and finished notification deliveries the [retention sweep](#limits-and-housekeeping) keeps; `0` keeps that history forever. |
| `TEND_DELIVERY_HOLD_MINUTES` | `15` | How long a notification that keeps failing may hold back the later ones on its channel, counted from its first attempt (see [Delivery](#alerting)); `0` turns per-channel ordering off. A whole number of minutes up to 1440; `serve` refuses anything else. |
| `TEND_TRUST_PROXY`   | `false`                 | Set to `1`, `true`, `yes`, or `on` when Tend runs behind a reverse proxy you control. [Rate limiting](#limits-and-housekeeping) then keys on the left-most `X-Forwarded-For` address instead of the TCP peer (an entry that is not an IP address is ignored). Only enable it when the proxy **overwrites** that header; if clients can reach Tend directly, or the proxy merely appends to the header, they can forge it and dodge the limit. |

`tend serve` **refuses to start without `TEND_MASTER_KEY`**, and with a key that is not 32 base64-encoded bytes: channel configs are encrypted with it, so a server without it could not send a single alert, and a typo in the variable name used to produce exactly that without a word. For a local experiment, `tend serve --no-master-key` starts anyway with alerts and the dashboard off (only `/healthz` and `/ping/{token}` are served) and says so in its banner. The master key serves two roles: it derives a stable session-cookie signing key (HKDF-SHA256, so restarts keep existing sessions valid) and it encrypts stored secrets and channel configs. See the [security model](docs/ARCHITECTURE.md#6-security-model) for details.

## SQLite vs Postgres

```sh
# SQLite (default): any value that is not a postgres URL is a SQLite file path.
TEND_DB=/data/tend.db tend serve

# Postgres: any value starting with postgres:// or postgresql:// switches backend.
TEND_DB=postgres://tend:secret@localhost:5432/tend?sslmode=disable tend serve
```

`TEND_DB` must be set; there is no default, and Tend refuses to open a database without it rather than create a `tend.db` in whatever directory it was started from. SQLite is appropriate for single-host deployments. Use Postgres when you want a managed/external database, `pg_dump` backups, or to run the database on a separate host from tend. (The job runner is single-instance; run one `tend serve` against a given database.) Tend runs migrations on every startup; for Postgres the database must already exist and the user must have DDL rights. Both backends have [identical behavior](docs/ARCHITECTURE.md#3-the-store-interface-and-sqlitepostgres-parity).

**Troubleshooting: the CLI shows nothing but the dashboard shows your jobs (or vice versa).** The CLI and the running server each resolve `TEND_DB` independently, so they must point at the *same* database. A *relative* SQLite path resolves against the directory the command is run from, so running a CLI command from a different directory (or against a container whose server uses `/data/tend.db`) reads a different, often empty, database. Use an absolute path, and run `tend doctor` to print the resolved driver, database path, org, base URL, and resource counts; the `tend serve` banner prints the same `db …(driver) org …`. For a containerized server, run CLI commands inside it, e.g. `docker exec <container> /tend doctor` or `docker exec <container> /tend heartbeat list`.

## Config-as-code (YAML)

Job definitions, notification channels, rules, and heartbeats can be managed declaratively. The repo ships a minimal [`jobs.yaml`](jobs.yaml) you can apply right away to see tend working:

```sh
tend sync jobs.yaml              # default: jobs absent from the file are DISABLED (prune=true)
tend sync -prune=false jobs.yaml # leave absent jobs untouched
```

`sync` is idempotent: the file is the source of truth, so re-running it converges the store to match. The file is parsed and validated in full before anything is written, so a bad entry, such as an invalid cron expression, aborts the sync and names the offending job. For the full set of options (http jobs, every schedule type, secrets, notification channels, and heartbeats), see the commented [`jobs.example.yaml`](jobs.example.yaml).

It prints a summary: `jobs(created=N updated=N disabled=N) channels=N rules=N heartbeats=N`.

```yaml
jobs:
  - name: nightly-backup          # required; unique name
    type: shell                   # shell | http
    command: "restic backup /data" # required for shell jobs
    cron: "0 3 * * *"             # one of: cron, interval_seconds, run_at
    timeout_seconds: 1800
    max_retries: 2
    env:                          # shell jobs only; inert on http jobs
      RESTIC_REPOSITORY: "{{ secret.restic_repo }}"   # resolved at sync time

  - name: health-poll
    type: http
    http_url: "https://example.com/health"
    http_method: GET              # default: GET
    interval_seconds: 300         # run every 300 s

  - name: one-off-migration
    type: shell
    command: "bin/migrate --apply"
    run_at: "2026-06-01T02:00:00Z"  # RFC3339; runs once at this time

notifications:
  channels:
    - name: ops-slack
      type: slack                 # webhook | slack | discord | smtp | telegram
      config:
        webhook_url: "{{ secret.slack_webhook }}"
  rules:
    - channel: ops-slack
      events: [run.failed, heartbeat.missed]  # all jobs
    - channel: ops-slack
      events: [run.failed]
      job: nightly-backup         # scoped to this job only

heartbeats:
  - name: external-backup
    period_seconds: 86400         # expected ping interval
    grace_seconds: 3600           # alert after period + grace
```

**Prune behaviour.** By default (`-prune=true`) any job present in the store but **absent from the YAML** is **disabled** (not deleted); its history is preserved. To hard-delete a job use `tend job rm <name>`. Pass `-prune=false` to leave absent jobs untouched, useful when the file covers only a subset of jobs.

**Secret references.** Channel `config` values and job `env` values may embed `{{ secret.NAME }}`. At `sync` time each placeholder is replaced with the decrypted secret. The plaintext never appears in the YAML.

## Secrets

Secrets are write-once via the CLI, stored encrypted, and never echoed. The value is read from stdin (never argv) to avoid process-list exposure:

```sh
printf 'the-secret-value' | tend secret set my_api_key
```

`TEND_MASTER_KEY` must be set when storing secrets. Reference them in job `env` and channel `config` as `{{ secret.NAME }}`. List stored secret names (never values) via `GET /api/secrets`.

## Alerting

Tend emits events; a **channel** is a delivery destination and a **rule** routes matching event types to a channel. These event types can be routed:

| Event | When | Payload |
|---|---|---|
| `run.failed` | a run failed, timed out, or was reaped (the precise status is in the payload) | `{"run_id","job_id","job_name","status","exit_code"}` |
| `run.recovered` | a run succeeded and the job's previous finished run had not | same as `run.failed` |
| `heartbeat.missed` | a heartbeat's `period + grace` passed without a ping | the heartbeat name |
| `heartbeat.recovered` | a ping arrived for a heartbeat that was down | the heartbeat name |
| `job.removed` | a job was deleted (`tend job rm`) | `{"job_id","job_name"}` |
| `heartbeat.removed` | a heartbeat was deleted (`tend heartbeat rm`) | the heartbeat name |
| `notification.failed` | a delivery was given up after 24 hours of failures | the type of the event that was not delivered |

`run.started` and `run.succeeded` are recorded (dashboard, `GET /api/events`) but never notify, so `tend rule add` and `tend sync` refuse a rule for them, or for any type not in the table, and list the valid ones. (`tend rule list` flags a rule stored by an older version that can never fire.) A `notification.failed` whose own delivery is given up is dropped rather than reported again, so route it to a different channel than the ones it reports on.

Channels are webhook, Slack, Discord, SMTP, or Telegram, and are managed via the CLI (`tend channel add`, config JSON on stdin) or YAML sync. Because channel config is encrypted at rest, `TEND_MASTER_KEY` must be set to create channels (syncing a config with channels while the key is unset is refused).

**Delivery.** When an event matches a rule, the notification is written to the database in the same transaction as the event and sent by a background worker inside `tend serve`, so a crash or a slow destination does not lose it. Each send is bounded (10 seconds for webhook, Slack, Discord and Telegram, 30 seconds for SMTP), so a destination that hangs fails that attempt instead of holding up the others. A failed send is retried with exponential backoff (1 second, doubling up to 5 minutes) for up to 24 hours; after that the delivery is marked failed and a `notification.failed` event is recorded. A destination that refuses the notification for good (an HTTP 4xx other than 408 and 429 from a webhook, Slack, Discord or Telegram, or an SMTP 5xx reply) is not retried: the delivery is given up at once, the same way. Each channel receives its notifications in order, one at a time: a notification waits until the ones queued before it on the same channel have been delivered or given up, so after an outage a receiver gets `heartbeat.missed` before the `heartbeat.recovered` that followed it (other channels are not held up). The wait is bounded: once a notification that keeps failing was first attempted more than 15 minutes ago (`TEND_DELIVERY_HOLD_MINUTES`), the ones behind it go out while it keeps retrying, so one notification a destination keeps failing cannot silence the channel; past that point their order is no longer guaranteed, and a receiver should order by the event's `CreatedAt`. Delivery is at-least-once, so a receiver can occasionally see the same notification twice. Sending needs `TEND_MASTER_KEY` (to decrypt the channel config); without it notifications stay queued. `tend run <name>` also drains the queue once before it exits, so a manual run notifies even when no server is running.

### Alert on a missed heartbeat

A heartbeat is a dead-man's-switch: register it, then have an external job ping `<TEND_BASE_URL>/ping/<token>` on its own schedule. If a ping does not arrive within `period + grace`, Tend emits `heartbeat.missed`; a later ping emits `heartbeat.recovered`. A heartbeat that has never been pinged is armed from the moment it is created, so a job that never sends its first ping is reported too. Route both to a channel with a rule:

```yaml
notifications:
  channels:
    - name: ops-slack
      type: slack
      config:
        webhook_url: "{{ secret.slack_webhook }}"
  rules:
    - channel: ops-slack
      events: [heartbeat.missed, heartbeat.recovered]
```

Make the ping retry, so a moment of unavailability (a restart, a network blip, or a `429 Too Many Requests`) does not turn into a missed heartbeat. curl's `--retry` treats 429 and 5xx answers as transient and honours `Retry-After`:

```sh
curl -fsS -m 10 --retry 5 --retry-max-time 60 -X POST "$TEND_BASE_URL/ping/<token>"
```

Recover a synced heartbeat's ping URL any time with `tend heartbeat ping-url <name>` (or `tend heartbeat show <name>`), and inspect its transition history with `tend heartbeat history <name>`.

### Send Tend events to Ward (or any sink)

To forward events to an external incident sink (a webhook receiver, an aggregator, or [Ward](https://github.com/marsadhq)), point a webhook channel at it and route the event types you care about:

```yaml
notifications:
  channels:
    - name: ward
      type: webhook
      config:
        url: "https://ward.example/ingest/tend"
  rules:
    - channel: ward
      events: [run.failed, run.recovered, heartbeat.missed, heartbeat.recovered,
               job.removed, heartbeat.removed, notification.failed]
```

The webhook payload is JSON with `subject`, `body`, and the full originating `event` (`ID`, `OrgID`, `Type`, `Source`, `Payload`, `DedupKey`, `CreatedAt`; the run events carry their dedup key, e.g. `run:42:run.recovered`, and `CreatedAt` is when Tend recorded the event, so a receiver can order and deduplicate what it gets). A channel receives its notifications in order, and at least once. Rules are org-wide; per-heartbeat routing is not yet supported.

## CLI reference

All commands share `TEND_DB` (and `TEND_MASTER_KEY` for secret-bearing commands). The entrypoint is `tend`; in Docker it is `/tend`.

| Command | Description |
|---------|-------------|
| `tend serve [--no-master-key]` | Start the job runner, HTTP server, heartbeat watcher, notification delivery worker, and daily retention sweep. Refuses to start without `TEND_MASTER_KEY` unless `--no-master-key` is passed (alerts and dashboard off). `tend serve -h` prints its usage and environment variables without opening the database. |
| `tend sync [-prune] <file>` | Reconcile jobs/channels/rules/heartbeats from YAML. |
| `tend version` | Print the binary version. |
| `tend help` (or `-h`, `--help`) | Print the command list. Like `version`, it works without `TEND_DB`. |
| `tend doctor` | Print the resolved driver, database, org, base URL, and resource counts (diagnose a CLI/server DB mismatch). |
| `tend job list` | List all jobs. |
| `tend job add [flags]` | Create a job (flags below). |
| `tend job enable <name>` | Enable a job. |
| `tend job disable <name>` | Disable a job. |
| `tend job rm <name>` | Hard-delete a job (and its runs + job-scoped rules). |
| `tend run <name>` | Run a job immediately (inline; prints result). |
| `tend logs <name> [-follow]` | Show the last 20 runs; `-follow` polls for new runs. |
| `tend secret set <key>` | Store a secret; value read from stdin. Requires `TEND_MASTER_KEY`. |
| `tend channel add -name <n> -type <t>` | Create/update a channel; JSON config read from stdin. Requires `TEND_MASTER_KEY`. |
| `tend channel list` | List channels (no config/credentials shown). |
| `tend rule add -channel <n> -event <e> [-job <n>]` | Create/update a notification rule. `-event` must be one of the [alertable event types](#alerting). |
| `tend rule list` | List rules. |
| `tend heartbeat add -name <n> -period <s> [-grace <s>]` | Create/update a heartbeat; prints the ping URL. |
| `tend heartbeat list` | List heartbeats and their current status. |
| `tend heartbeat show <name>` | Show a heartbeat's status, period, grace, last-seen, and ping URL. |
| `tend heartbeat ping-url <name>` | Print a heartbeat's ping URL (recover a synced heartbeat's token). |
| `tend heartbeat history <name> [-limit <n>]` | Show a heartbeat's missed/recovered transitions (default 20). |
| `tend heartbeat rm <name>` | Delete a heartbeat (its past events are kept). |
| `tend user add -email <e>` | Create an admin user; password read from stdin. |
| `tend token create -name <n>` | Create an API token; printed once. |
| `tend token list` | List API tokens (names and IDs; hash never shown). |
| `tend token revoke -id <n>` | Revoke an API token by ID. |

`tend job add` flags: `-name` (required); `-type shell|http` (default `shell`); `-command` (required for shell); `-url` (required for http); `-method` (default `GET`, http only); `-body` (http only); `-cron`, `-interval <seconds>`, `-run-at <RFC3339>` (mutually exclusive; a cron expression that does not parse is rejected and no job is created); `-timeout <seconds>` per attempt (default `0` = 30 minutes); `-max-retries <n>` (default `0`); `-env KEY=VALUE` (repeatable; shell jobs only in effect).

`tend channel add` accepts `-type` of `webhook`, `slack`, `discord`, `smtp`, or `telegram` (Telegram config is `{"bot_token": "...", "chat_id": "..."}`). The heartbeat ping URL is `<TEND_BASE_URL>/ping/<token>`; send a GET or POST to it from any external job. There is no `user list` or `user rm` in this release.

## HTTP API

All `/api/...` routes require authentication via a session cookie (browser login) or an `Authorization: Bearer <token>` header (token created with `tend token create`).

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/jobs` | List jobs (`?limit=`, max 200). |
| `GET` | `/api/jobs/{id}` | Get a job. |
| `GET` | `/api/jobs/{id}/runs` | List runs for a job (`?limit=`, max 200). |
| `GET` | `/api/runs/{id}` | Get a run (includes full output). |
| `GET` | `/api/channels` | List channels (metadata only; no config/credentials). |
| `GET` | `/api/rules` | List notification rules. |
| `GET` | `/api/heartbeats` | List heartbeats (no ping token). |
| `GET` | `/api/events` | List recent events (`?limit=`, max 200). |
| `GET` | `/api/secrets` | List secret names and timestamps (no values). |
| `POST` | `/api/jobs/{id}/run` | Enqueue a run. Returns `202 {"run_id": N}`. |
| `POST` | `/api/jobs/{id}/enable` | Enable a job. Returns the updated job. |
| `POST` | `/api/jobs/{id}/disable` | Disable a job. Returns the updated job. |

Two routes are unauthenticated: `GET /healthz` returns `ok` (suitable for load-balancer checks), and `GET`/`POST` `/ping/{token}` is the heartbeat ping receiver. `/ping/{token}` and `POST /login` are rate limited; see [Limits and housekeeping](#limits-and-housekeeping).

The API is **read-mostly by design**: resource definitions are managed via the CLI and config-as-code; only run-now, enable, and disable are exposed as mutations. See the [capability asymmetries](docs/ARCHITECTURE.md#7-capability-asymmetries-by-design) for the rationale.

## Limits and housekeeping

These are built in; only the retention windows are configurable.

| What | Behavior |
|------|----------|
| Captured output | At most 1 MiB per attempt (stdout and stderr combined for shell jobs, the response body for HTTP jobs). The rest is discarded and the output ends with `... [output truncated at 1MiB]`; the cut never splits a UTF-8 character. The run's status and exit code are not affected. When a job retries, the run stores the output of its last attempt. Output is stored as text on either database: NUL bytes are dropped and bytes that are not valid UTF-8 are replaced with U+FFFD, before secret values are redacted. What is stored, the `HTTP <status>` line of an HTTP job included, never exceeds 1 MiB plus that marker. |
| Job timeout | A job without a timeout (`timeout_seconds` / `-timeout` of 0) gets 30 minutes per attempt. A negative timeout is refused by `tend job add` and `tend sync`. |
| Stale runs | On startup, runs a crashed `tend serve` left `running` are put back in the queue. A run that a `tend run` command is executing is left to it, so starting the server meanwhile never runs that job twice; if the command died, the sweep below fails the run. While `serve` is up, a sweep every minute looks for runs still `running` more than 2 minutes past the longest they could legitimately take, counted from the start of the run: every attempt (`max_retries` + 1 of them) reaching its timeout, plus the pauses between retries. For a run the server itself is executing, the timeout and retry count are the ones the job had when the run started, so editing a job cannot get its run in flight failed; for any other run they are the job's current ones. Such a run is presumed to have lost its worker: it is marked failed and `run.failed` is emitted, so the job is not blocked until the next restart. That result is final; if the worker turns up after all, its late result is discarded, which the server logs. |
| Retention | `serve` prunes at startup and then every 24 hours: events older than 30 days, finished job runs older than 90 days, and finished (delivered or failed) notification deliveries older than 7 days. Set `TEND_RETENTION_EVENTS_DAYS`, `TEND_RETENTION_RUNS_DAYS` and `TEND_RETENTION_DELIVERIES_DAYS` to a whole number of days to change a window, or to `0` to keep that history forever; `serve` refuses to start on any other value, and its banner and `tend doctor` print the windows in effect. Pending and running runs, pending deliveries, and the events they refer to are never pruned. |
| Disk space (SQLite) | When a sweep has freed at least 8 MiB and a quarter of the database file, `serve` gives the space back to the file system. The first time, it switches the file to incremental auto-vacuum with one `VACUUM`, which rewrites the file and holds writes for a moment (a few seconds at most for the 512 MiB it will do this for while serving); from then on it frees space in small incremental steps. A larger file that has never been compacted is left alone and logged: stop `tend serve` and run `sqlite3 /path/to/tend.db VACUUM` once. Postgres reuses the space of deleted rows itself (autovacuum). |
| Rate limits | `POST /login`: a burst of 5 requests, then 1 per second, per client address. `/ping/{token}`: pings for a real heartbeat are limited per heartbeat (a burst of 10, then 1 per second) and never per address, so one host can ping any number of heartbeats on the same minute; pings with an unknown token are limited per client address (a burst of 30, then 1 per second), and an address over that limit is refused for every token until it refills, which is what stops token guessing. Limits are held in memory by the server process. Requests over a limit get `429 Too Many Requests` with a `Retry-After` header; ping clients should retry (see [the heartbeat section](#alert-on-a-missed-heartbeat)). Behind a reverse proxy every client shares the proxy's address unless `TEND_TRUST_PROXY` is set. |
| HTTP timeouts | The server gives up on a connection after 10 seconds spent reading a request (headers and body), writing a response, or sitting idle. |
| Security headers | Every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, and a `Content-Security-Policy` that only allows same-origin scripts, styles, and images. `Strict-Transport-Security` is not sent because Tend itself serves plain HTTP; add it at the proxy that terminates TLS. |
| URLs in errors | When a webhook, Slack, Discord, or Telegram delivery or an HTTP job fails, the error names only the host. The rest of the URL, where tokens usually live, is kept out of logs and stored run output. |

## Backups

**SQLite**: back up the database file **and** its WAL/SHM sidecars, or use the online backup API:

```sh
# Stopped: copy all three files
cp tend.db tend.db-wal tend.db-shm /backup/

# Online: SQLite's built-in backup (consistent without stopping)
sqlite3 tend.db ".backup /backup/tend-$(date +%Y%m%d).db"
```

Copying only `tend.db` while a `tend.db-wal` holds uncommitted pages produces an inconsistent backup; always include `-wal` and `-shm`, or use `sqlite3 .backup`. In Docker the database lives at `/data/tend.db` inside the `tenddata` volume:

```sh
docker run --rm -v tenddata:/data -v /backup:/backup \
  alpine sh -c 'cp /data/tend.db /data/tend.db-wal /data/tend.db-shm /backup/ 2>/dev/null; true'
```

**Postgres:**

```sh
pg_dump -h localhost -U tend -d tend -F c -f tend-$(date +%Y%m%d).pgdump
```

## Consuming Tend from another service

The HTTP API is designed to be driven by other services in the same stack. Create a dedicated token, then use the action endpoints to trigger jobs or toggle state:

```sh
tend token create -name my-service   # prints the token once; store it securely

# Run a job now (returns 202 with run_id)
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/jobs/42/run

# Poll the result
curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/runs/<run_id>
```

Only run-now, enable, and disable are available as API mutations; creating, editing, and deleting resources is CLI- and config-file-driven to prevent drift against the declared config.

---

## Documentation and links

- [Architecture](docs/ARCHITECTURE.md): package layout, store parity, event pipeline, security model, and intentional capability asymmetries.
- [Contributing](CONTRIBUTING.md): how to build, test, and submit changes.
- [Security policy](SECURITY.md): how to report vulnerabilities.
- [License](LICENSE): AGPL-3.0.
- [Contributor License Agreement](CLA.md): required for contributions.
