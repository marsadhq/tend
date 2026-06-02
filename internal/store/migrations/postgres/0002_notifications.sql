ALTER TABLE heartbeats ADD COLUMN token TEXT;
ALTER TABLE heartbeats ADD COLUMN period_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE heartbeats ADD COLUMN grace_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE heartbeats ADD COLUMN status TEXT NOT NULL DEFAULT 'new';
CREATE UNIQUE INDEX idx_heartbeats_token ON heartbeats(token);

ALTER TABLE notification_rules ADD COLUMN job_id INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX idx_notification_rules_uniq ON notification_rules(org_id, channel_id, event_type, job_id);
