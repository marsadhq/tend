-- 0011_ping_body_bytes: each heartbeat's ping log is held to a byte budget
-- as well as an entry count. body_bytes is a ping's body size in bytes, kept
-- in an index with (heartbeat_id, id) so the budget is summed from the index
-- alone; that index also serves what idx_heartbeat_pings_heartbeat did.
ALTER TABLE heartbeat_pings ADD COLUMN body_bytes INTEGER NOT NULL DEFAULT 0;
UPDATE heartbeat_pings SET body_bytes = LENGTH(CAST(body AS BLOB)) WHERE body IS NOT NULL;
DROP INDEX idx_heartbeat_pings_heartbeat;
CREATE INDEX idx_heartbeat_pings_bytes ON heartbeat_pings(heartbeat_id, id, body_bytes);
