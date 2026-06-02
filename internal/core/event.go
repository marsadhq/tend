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
}
