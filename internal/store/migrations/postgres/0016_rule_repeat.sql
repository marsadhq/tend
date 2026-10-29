-- 0016_rule_repeat: how often a run.failed rule reminds its channel while a
-- job keeps failing, in seconds: NULL (every rule before this, and any that
-- sets none) for the channel type's default, an hour for the channels people
-- read and every failure for a webhook; 0 for every failure.
ALTER TABLE notification_rules ADD COLUMN repeat_seconds INTEGER;
