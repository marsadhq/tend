package heartbeat

import (
	"encoding/json"
	"time"

	"github.com/marsadhq/tend/internal/core"
)

// Reasons a heartbeat goes down other than a plain miss, as MissDetail.Reason.
// A heartbeat.missed without a detail is a plain miss: no ping arrived within
// period + grace.
const (
	// ReasonFailed: a failure ping (/fail, or a non-zero exit status) took
	// the heartbeat down at once.
	ReasonFailed = "failed"
	// ReasonOverran: a run was started (/start) and no success or failure
	// arrived within the grace that followed.
	ReasonOverran = "overran"
)

// MissDetail is the Detail of a heartbeat.missed that is more than a plain
// miss. It says why the heartbeat went down so the alert can say it; the
// event's payload stays the plain heartbeat name either way.
type MissDetail struct {
	Reason string `json:"reason"`
	// ExitStatus is the exit status a failure ping reported, nil for /fail.
	ExitStatus *int `json:"exit_status,omitempty"`
	// Output is the tail of the failure ping's logged body (OutputTail).
	Output string `json:"output,omitempty"`
	// StartedAt and GraceSeconds describe the run that overran: when it
	// started and how long it was allowed (Heartbeat.RunGrace).
	StartedAt    time.Time `json:"started_at,omitzero"`
	GraceSeconds int       `json:"grace_seconds,omitempty"`
}

// The tail of a failure ping's body that its alert carries: its last
// OutputTailLines lines, and at most OutputTailBytes of them (core.OutputTail).
// The whole body stays in the heartbeat's ping log.
const (
	OutputTailLines = core.OutputTailLines
	OutputTailBytes = core.OutputTailBytes
)

// FailedEvent is the heartbeat.missed recorded when a failure ping takes the
// heartbeat named name down. exitStatus is the status the ping reported (nil
// for /fail) and body what it logged.
func FailedEvent(orgID int64, name string, exitStatus *int, body string) core.Event {
	ev := MissedEvent(orgID, name)
	ev.Detail = encodeDetail(MissDetail{Reason: ReasonFailed, ExitStatus: exitStatus, Output: OutputTail(body)})
	return ev
}

// OverranEvent is the heartbeat.missed recorded when a run of the heartbeat
// named name, started at startedAt, did not finish within graceSeconds (the
// heartbeat's RunGrace).
func OverranEvent(orgID int64, name string, startedAt time.Time, graceSeconds int) core.Event {
	ev := MissedEvent(orgID, name)
	ev.Detail = encodeDetail(MissDetail{Reason: ReasonOverran, StartedAt: startedAt.UTC(), GraceSeconds: graceSeconds})
	return ev
}

func encodeDetail(d MissDetail) string {
	b, err := json.Marshal(d)
	if err != nil {
		return "" // cannot happen for this struct; a plain miss is the fallback
	}
	return string(b)
}

// ParseMissDetail decodes the Detail of a heartbeat.missed. It reports false
// for a plain miss (no detail) and for anything that is not a detail with a
// reason.
func ParseMissDetail(detail string) (MissDetail, bool) {
	if detail == "" {
		return MissDetail{}, false
	}
	var d MissDetail
	if err := json.Unmarshal([]byte(detail), &d); err != nil || d.Reason == "" {
		return MissDetail{}, false
	}
	return d, true
}

// OutputTail returns the end of a logged body for an alert (core.OutputTail):
// its last OutputTailLines lines, trailing blank space dropped, at most
// OutputTailBytes long and cut on a character boundary. Blank output has an
// empty tail.
func OutputTail(s string) string { return core.OutputTail(s) }
