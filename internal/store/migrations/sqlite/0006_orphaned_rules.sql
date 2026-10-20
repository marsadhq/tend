-- 0006_orphaned_rules: deleting a channel used to leave its notification rules
-- behind. They kept matching, and every match queued a delivery to a channel
-- that no longer existed, retried for 24 hours. DeleteChannel now removes the
-- rules with the channel and the enqueue only considers existing channels;
-- this clears what earlier versions left: rules without their channel, and the
-- pending deliveries to channels that are gone (finalized as failed).
DELETE FROM notification_rules
 WHERE NOT EXISTS (
   SELECT 1 FROM notification_channels c
    WHERE c.id = notification_rules.channel_id AND c.org_id = notification_rules.org_id
 );
UPDATE deliveries SET state = 'failed'
 WHERE state = 'pending'
   AND NOT EXISTS (SELECT 1 FROM notification_channels c WHERE c.id = deliveries.channel_id);
