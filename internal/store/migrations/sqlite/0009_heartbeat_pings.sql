-- 0009_heartbeat_pings: the Healthchecks-compatible ping API. A heartbeat
-- remembers the run it is timing (started by /start, ended by a success or a
-- failure; started_rid is its ?rid=, '' for none), and every ping is logged:
-- its kind (success, start, fail or log), the exit status it reported, its
-- rid, the run's duration when it completed one, and the request body.
ALTER TABLE heartbeats ADD COLUMN started_at TEXT;
ALTER TABLE heartbeats ADD COLUMN started_rid TEXT NOT NULL DEFAULT '';

CREATE TABLE heartbeat_pings (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id       INTEGER NOT NULL,
    heartbeat_id INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    exit_status  INTEGER,
    rid          TEXT NOT NULL DEFAULT '',
    duration_ms  INTEGER,
    body         TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX idx_heartbeat_pings_heartbeat ON heartbeat_pings(heartbeat_id, id);
CREATE INDEX idx_heartbeat_pings_created ON heartbeat_pings(created_at);
