-- 0008_event_detail: optional JSON detail on an event, for what its type and
-- payload do not say: a heartbeat.missed caused by a failure ping carries the
-- exit status and the tail of the logged output, so the alert can say what
-- happened. The payload stays the plain heartbeat name.
ALTER TABLE events ADD COLUMN detail TEXT;
