-- 0005_deliveries_per_channel: a channel's deliveries go out in order, one at
-- a time: ClaimDueDeliveries only claims a pending delivery when no older
-- delivery for the same channel is still pending. This index serves that
-- "older pending delivery for this channel" check.
CREATE INDEX idx_deliveries_channel_pending ON deliveries(channel_id, id) WHERE state = 'pending';
