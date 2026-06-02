-- 0001_init: foundational schema for Tend (Postgres dialect).
-- Mirrors the SQLite schema: timestamps are TEXT in RFC3339 (UTC), flags/counters
-- are INTEGER, env is TEXT (JSON). This keeps the shared scan/helper code dialect
-- agnostic. Every tenant-scoped table carries org_id.

CREATE TABLE orgs (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    org_id        BIGINT NOT NULL,
    email         TEXT NOT NULL,
    password_hash TEXT,
    created_at    TEXT NOT NULL,
    UNIQUE(org_id, email)
);

CREATE TABLE memberships (
    id         BIGSERIAL PRIMARY KEY,
    org_id     BIGINT NOT NULL,
    user_id    BIGINT NOT NULL,
    role       TEXT NOT NULL DEFAULT 'member',
    created_at TEXT NOT NULL,
    UNIQUE(org_id, user_id)
);

CREATE TABLE api_tokens (
    id         BIGSERIAL PRIMARY KEY,
    org_id     BIGINT NOT NULL,
    name       TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(org_id, token_hash)
);

CREATE TABLE jobs (
    id               BIGSERIAL PRIMARY KEY,
    org_id           BIGINT NOT NULL,
    name             TEXT NOT NULL,
    type             TEXT NOT NULL,
    command          TEXT,
    http_url         TEXT,
    http_method      TEXT,
    http_body        TEXT,
    cron             TEXT,
    interval_seconds INTEGER NOT NULL DEFAULT 0,
    run_at           TEXT,
    timeout_seconds  INTEGER NOT NULL DEFAULT 0,
    max_retries      INTEGER NOT NULL DEFAULT 0,
    enabled          INTEGER NOT NULL DEFAULT 1,
    env              TEXT,
    next_run         TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    UNIQUE(org_id, name)
);

CREATE TABLE job_runs (
    id         BIGSERIAL PRIMARY KEY,
    org_id     BIGINT NOT NULL,
    job_id     BIGINT NOT NULL,
    status     TEXT NOT NULL,
    attempt    INTEGER NOT NULL DEFAULT 1,
    exit_code  INTEGER NOT NULL DEFAULT 0,
    output     TEXT,
    claimed_by TEXT,
    started_at TEXT,
    ended_at   TEXT,
    created_at TEXT NOT NULL
);

CREATE INDEX idx_job_runs_pending ON job_runs(status, id) WHERE status IN ('pending', 'running');
CREATE INDEX idx_job_runs_org_job ON job_runs(org_id, job_id);

CREATE TABLE secrets (
    id         BIGSERIAL PRIMARY KEY,
    org_id     BIGINT NOT NULL,
    name       TEXT NOT NULL,
    ciphertext TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(org_id, name)
);

CREATE TABLE heartbeats (
    id           BIGSERIAL PRIMARY KEY,
    org_id       BIGINT NOT NULL,
    name         TEXT NOT NULL,
    last_seen_at TEXT,
    created_at   TEXT NOT NULL,
    UNIQUE(org_id, name)
);

CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    org_id     BIGINT NOT NULL,
    type       TEXT NOT NULL,
    source     TEXT,
    payload    TEXT,
    dedup_key  TEXT,
    created_at TEXT NOT NULL
);

CREATE INDEX idx_events_org_id ON events(org_id, id);

CREATE TABLE notification_channels (
    id         BIGSERIAL PRIMARY KEY,
    org_id     BIGINT NOT NULL,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL,
    config     TEXT,
    created_at TEXT NOT NULL,
    UNIQUE(org_id, name)
);

CREATE TABLE notification_rules (
    id          BIGSERIAL PRIMARY KEY,
    org_id      BIGINT NOT NULL,
    channel_id  BIGINT NOT NULL,
    event_type  TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL
);
