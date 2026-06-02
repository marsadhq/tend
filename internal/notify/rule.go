package notify

import "time"

// Rule binds an event type to a notification channel for an org. When an
// alertable event is dispatched, every enabled Rule whose EventType matches and
// whose JobID is either 0 (all jobs) or equal to the event's job fans the alert
// out to its channel.
//
// JobID == 0 means "all jobs"; a real job ID is >= 1. The store enforces
// uniqueness on (org_id, channel_id, event_type, job_id).
type Rule struct {
	ID        int64
	OrgID     int64
	ChannelID int64
	EventType string
	Enabled   bool
	JobID     int64
	CreatedAt time.Time
}
