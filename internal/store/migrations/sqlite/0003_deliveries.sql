-- 0003_deliveries: durable notification delivery queue. One row per
-- (event, channel) notification, enqueued in the SAME transaction as the
-- event insert (EmitEvent / FinishRunAndEmit / ReapStaleRun), drained by the
-- notify worker with capped exponential backoff. A process crash or slow
-- destination can therefore never lose a matched notification silently.
CREATE TABLE deliveries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id          INTEGER NOT NULL,
    event_id        INTEGER NOT NULL,
    channel_id      INTEGER NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    state           TEXT NOT NULL DEFAULT 'pending',   -- pending|delivered|failed
    next_attempt_at TEXT NOT NULL,
    created_at      TEXT NOT NULL
);
CREATE INDEX idx_deliveries_due ON deliveries(state, next_attempt_at);
