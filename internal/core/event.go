package core

import "time"

// Event is the generic record carried by the suite's event pipeline (the spine
// reused by later milestones). The jobs runner emits run.* events.
type Event struct {
	ID        int64
	OrgID     int64
	Type      string // e.g. "run.started", "run.succeeded", "run.failed"
	Source    string // e.g. "jobs.runner"
	Payload   string // JSON
	DedupKey  string
	CreatedAt time.Time
	// Detail is optional JSON saying more about what happened than Type and
	// Payload do, for the notification text: why a heartbeat went down, say.
	// It is stored with the event but is not part of the webhook's legacy
	// "event" object, whose keys never change: this struct's JSON form is that
	// object, so Detail is left out of it.
	Detail string `json:"-"`
}
