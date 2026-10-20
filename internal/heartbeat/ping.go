package heartbeat

import (
	"time"

	"github.com/marsadhq/tend/internal/core"
)

// Ping kinds, as a heartbeat's ping log records them. They follow the
// Healthchecks ping API: a success (/ping/<token>, or exit status 0), a start
// (/start), a failure (/fail, or exit status 1 to 255), and a log entry
// (/log), which changes nothing.
const (
	PingSuccess = "success"
	PingStart   = "start"
	PingFail    = "fail"
	PingLog     = "log"
)

// ValidPingKind reports whether kind is one of the ping kinds.
func ValidPingKind(kind string) bool {
	switch kind {
	case PingSuccess, PingStart, PingFail, PingLog:
		return true
	}
	return false
}

// MaxRunDuration is how long after its start a completion is still paired
// with it for a duration, as in Healthchecks: a completion later than this is
// taken to be unrelated to the start.
const MaxRunDuration = 72 * time.Hour

// PingInput is one ping as a client sent it.
type PingInput struct {
	Kind string // PingSuccess, PingStart, PingFail or PingLog
	// ExitStatus is the status of an exit-status ping (/0 to /255), nil for
	// every other ping.
	ExitStatus *int
	// RID is the run id from ?rid= (a lower-case UUID), "" when none. A
	// start and the completion that carries the same RID are one run.
	RID string
	// Body is the request body, already cleaned and capped, "" when none.
	Body string
}

// Ping is one entry of a heartbeat's ping log.
type Ping struct {
	ID          int64
	OrgID       int64
	HeartbeatID int64
	Kind        string
	ExitStatus  *int   // see PingInput.ExitStatus
	RID         string // see PingInput.RID
	// Duration is, for a success or failure paired with its start, the time
	// since that start; nil otherwise.
	Duration  *time.Duration
	Body      string
	CreatedAt time.Time
}

// PingResult is what recording a ping did.
type PingResult struct {
	OrgID       int64
	HeartbeatID int64
	Name        string
	// Event is the transition the ping recorded, in the same transaction:
	// heartbeat.missed for a failure that took the heartbeat down, or
	// heartbeat.recovered for a success that brought it back up. It is nil
	// when the ping changed no status that alerts.
	Event *core.Event
}
