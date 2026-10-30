-- 0017_failure_alerts: what each channel was told about each job's failures
-- (notify.AlertState), so a failing job alerts once, reminds at its rule's
-- interval and sends its recovery only where its failure went. last_sent_at
-- is when the channel was last sent one of the job's run.failed (its next
-- reminder comes an interval after it), and held_event_id a run.failed of a
-- streak the channel has not heard of, sent when its hold ends (NULL: none).
-- A row goes with its job and with its channel.
CREATE TABLE failure_alerts (
    org_id        INTEGER NOT NULL,
    channel_id    INTEGER NOT NULL,
    job_id        INTEGER NOT NULL,
    last_sent_at  TEXT,
    held_event_id INTEGER,
    PRIMARY KEY (channel_id, job_id)
);
CREATE INDEX idx_failure_alerts_job ON failure_alerts(job_id);
CREATE INDEX idx_failure_alerts_held ON failure_alerts(held_event_id) WHERE held_event_id IS NOT NULL;
