-- 0007_delivery_first_attempt: when a delivery was first attempted. A
-- channel's oldest pending delivery holds back the ones behind it (ordered
-- delivery), but only for a bounded time after its first attempt; past that,
-- later deliveries on the channel are claimed while it keeps retrying until
-- its own give-up. Deliveries already attempted when this runs are dated from
-- their creation, which is when their first attempt was made.
ALTER TABLE deliveries ADD COLUMN first_attempt_at TEXT;
UPDATE deliveries SET first_attempt_at = created_at WHERE state = 'pending' AND attempts > 0;
