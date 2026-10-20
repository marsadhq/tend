-- 0010_org_ping_key: an org's ping key, for the slug form of the ping API
-- (/ping/<ping-key>/<slug>, as in Healthchecks), where <slug> is a heartbeat's
-- name. NULL means the org has none and the slug form is off. A key resolves
-- to exactly one org.
ALTER TABLE orgs ADD COLUMN ping_key TEXT;
CREATE UNIQUE INDEX idx_orgs_ping_key ON orgs(ping_key);
