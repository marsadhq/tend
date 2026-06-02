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
- **Run monitoring**: every execution is recorded with its status, exit code, timing, and full captured output, browsable in the dashboard or over the HTTP API.
- **Alerting**: route `run.failed`, `heartbeat.missed`, and other events to webhook, Slack, Discord, or SMTP channels, globally or scoped to a single job.
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

For unattended operation see [`deploy/tend.service`](deploy/tend.service) (a hardened systemd unit) and [`deploy/docker-compose.yml`](deploy/docker-compose.yml).

---

# Reference

## Environment variables

| Variable             | Default                 | Description |
|----------------------|-------------------------|-------------|
| `TEND_DB`            | `tend.db`               | SQLite file path **or** a `postgres://` / `postgresql://` URL. (In the Docker image the default is `/data/tend.db`.) |
| `TEND_MASTER_KEY`    | *(none)*                | Base64-encoded 32-byte key. Required for the dashboard, session signing, and secrets encryption. Generate once with `head -c 32 /dev/urandom \| base64`. **Must be stable** across restarts; changing it invalidates all sessions and makes all stored secrets unreadable. |
| `TEND_ADDR`          | `:8080`                 | TCP listen address for the HTTP server. |
| `TEND_BASE_URL`      | `http://localhost:8080` | Externally-reachable base URL. Used when printing heartbeat ping URLs. Set this when Tend is behind a reverse proxy. |
| `TEND_COOKIE_SECURE` | `false`                 | Set to `1`, `true`, `yes`, or `on` to mark session cookies as `Secure` (HTTPS-only). Leave `false` when TLS is terminated at a reverse proxy. |

Without `TEND_MASTER_KEY` the server starts in **public-only mode**: the dashboard, `/login`, secrets, and notification channels are all disabled. The master key serves two roles: it derives a stable session-cookie signing key (HKDF-SHA256, so restarts keep existing sessions valid) and it encrypts stored secrets and channel configs. See the [security model](docs/ARCHITECTURE.md#6-security-model) for details.

## SQLite vs Postgres

```sh
# SQLite (default): any value that is not a postgres URL is a SQLite file path.
TEND_DB=/data/tend.db tend serve

# Postgres: any value starting with postgres:// or postgresql:// switches backend.
TEND_DB=postgres://tend:secret@localhost:5432/tend?sslmode=disable tend serve
```

When `TEND_DB` is unset it defaults to `tend.db` in the current directory. SQLite is appropriate for single-host deployments. Use Postgres when you want a managed/external database, `pg_dump` backups, or to run the database on a separate host from tend. (The job runner is single-instance; run one `tend serve` against a given database.) Tend runs migrations on every startup; for Postgres the database must already exist and the user must have DDL rights. Both backends have [identical behavior](docs/ARCHITECTURE.md#3-the-store-interface-and-sqlitepostgres-parity).

## Config-as-code (YAML)

Job definitions, notification channels, rules, and heartbeats can be managed declaratively. The repo ships a minimal [`jobs.yaml`](jobs.yaml) you can apply right away to see tend working:

```sh
tend sync jobs.yaml              # default: jobs absent from the file are DISABLED (prune=true)
tend sync -prune=false jobs.yaml # leave absent jobs untouched
```

`sync` is idempotent: the file is the source of truth, so re-running it converges the store to match. For the full set of options (http jobs, every schedule type, secrets, notification channels, and heartbeats), see the commented [`jobs.example.yaml`](jobs.example.yaml).

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
      type: slack                 # webhook | slack | discord | smtp
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

## CLI reference

All commands share `TEND_DB` (and `TEND_MASTER_KEY` for secret-bearing commands). The entrypoint is `tend`; in Docker it is `/tend`.

| Command | Description |
|---------|-------------|
| `tend serve` | Start the job runner, HTTP server, and heartbeat watcher. |
| `tend sync [-prune] <file>` | Reconcile jobs/channels/rules/heartbeats from YAML. |
| `tend version` | Print the binary version. |
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
| `tend rule add -channel <n> -event <e> [-job <n>]` | Create/update a notification rule. |
| `tend rule list` | List rules. |
| `tend heartbeat add -name <n> -period <s> [-grace <s>]` | Create/update a heartbeat; prints the ping URL. |
| `tend heartbeat list` | List heartbeats and their current status. |
| `tend user add -email <e>` | Create an admin user; password read from stdin. |
| `tend token create -name <n>` | Create an API token; printed once. |
| `tend token list` | List API tokens (names and IDs; hash never shown). |
| `tend token revoke -id <n>` | Revoke an API token by ID. |

`tend job add` flags: `-name` (required); `-type shell|http` (default `shell`); `-command` (required for shell); `-url` (required for http); `-method` (default `GET`, http only); `-body` (http only); `-cron`, `-interval <seconds>`, `-run-at <RFC3339>` (mutually exclusive); `-timeout <seconds>` (default `0` = no limit); `-max-retries <n>` (default `0`); `-env KEY=VALUE` (repeatable; shell jobs only in effect).

`tend channel add` accepts `-type` of `webhook`, `slack`, `discord`, or `smtp`. The heartbeat ping URL is `<TEND_BASE_URL>/ping/<token>`; send a GET or POST to it from any external job. There is no `user list` or `user rm` in this release.

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

Two routes are unauthenticated: `GET /healthz` returns `ok` (suitable for load-balancer checks), and `GET`/`POST` `/ping/{token}` is the heartbeat ping receiver.

The API is **read-mostly by design**: resource definitions are managed via the CLI and config-as-code; only run-now, enable, and disable are exposed as mutations. See the [capability asymmetries](docs/ARCHITECTURE.md#7-capability-asymmetries-by-design) for the rationale.

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
