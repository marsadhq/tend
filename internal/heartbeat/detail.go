package heartbeat

import (
	"encoding/json"
	"time"

	"github.com/marsadhq/tend/internal/core"
)

// Why a heartbeat went down, as MissDetail.Reason. A heartbeat.missed with no
// detail (recorded before misses carried one) or with a reason this version
// does not know reads as a plain miss.
const (
	// ReasonMissed: no ping arrived within period + grace (a plain miss).
	ReasonMissed = "missed"
	// ReasonFailed: a failure ping (/fail, or a non-zero exit status) took
	// the heartbeat down at once.
	ReasonFailed = "failed"
	// ReasonOverran: a run was started (/start) and no success or failure
	// arrived within the grace that followed.
	ReasonOverran = "overran"
)

// MissDetail is the Detail of a heartbeat.missed. It says why the heartbeat
// went down, and what the alert needs to say so, while the event's payload
// stays the plain heartbeat name.
type MissDetail struct {
	Reason string `json:"reason"`
	// HeartbeatID is the heartbeat's id, for a link to its page.
	HeartbeatID int64 `json:"heartbeat_id,omitempty"`
	// ExitStatus is the exit status a failure ping reported, nil for /fail.
	ExitStatus *int `json:"exit_status,omitempty"`
	// Output is the tail of the failure ping's logged body: its last
	// core.MaxOutputTailLines lines, at most core.OutputTailBytes, of which
	// an alert shows as many lines as it is set to.
	Output string `json:"output,omitempty"`
	// LastSeenAt is a plain miss's last ping, zero when it never pinged.
	LastSeenAt time.Time `json:"last_seen_at,omitzero"`
	// PeriodSeconds is a plain miss's period.
	PeriodSeconds int `json:"period_seconds,omitempty"`
	// StartedAt is when the run that overran started.
	StartedAt time.Time `json:"started_at,omitzero"`
	// GraceSeconds is a plain miss's grace, or how long the run that overran
	// was allowed (Heartbeat.RunGrace).
	GraceSeconds int `json:"grace_seconds,omitempty"`
}

// The tail of a failure ping's body that its alert carries: its last
// OutputTailLines lines, and at most OutputTailBytes of them (core.OutputTail).
// The whole body stays in the heartbeat's ping log.
const (
	OutputTailLines = core.OutputTailLines
	OutputTailBytes = core.OutputTailBytes
)

// FailedEvent is the heartbeat.missed recorded when a failure ping takes the
// heartbeat id, named name, down. exitStatus is the status the ping reported
// (nil for /fail) and body what it logged.
func FailedEvent(orgID, id int64, name string, exitStatus *int, body string) core.Event {
	ev := MissedEvent(orgID, name)
	ev.Detail = encodeDetail(MissDetail{Reason: ReasonFailed, HeartbeatID: id, ExitStatus: exitStatus,
		Output: core.TailLines(body, core.MaxOutputTailLines)})
	return ev
}

// OverdueEvent is the heartbeat.missed the watcher records when no ping of
// hb arrived within its period + grace: when it last pinged (nothing when it
// never did) and what it expects.
func OverdueEvent(hb Heartbeat) core.Event {
	ev := MissedEvent(hb.OrgID, hb.Name)
	ev.Detail = encodeDetail(MissDetail{Reason: ReasonMissed, HeartbeatID: hb.ID, LastSeenAt: hb.LastSeenAt.UTC(),
		PeriodSeconds: hb.PeriodSeconds, GraceSeconds: hb.GraceSeconds})
	return ev
}

// OverranEvent is the heartbeat.missed recorded when the run hb started (at
// hb.StartedAt) did not finish within the grace it is allowed (RunGrace).
func OverranEvent(hb Heartbeat) core.Event {
	ev := MissedEvent(hb.OrgID, hb.Name)
	ev.Detail = encodeDetail(MissDetail{Reason: ReasonOverran, HeartbeatID: hb.ID, StartedAt: hb.StartedAt.UTC(),
		GraceSeconds: int(hb.RunGrace() / time.Second)})
	return ev
}

func encodeDetail(d MissDetail) string {
	b, err := json.Marshal(d)
	if err != nil {
		return "" // cannot happen for this struct; a plain miss is the fallback
	}
	return string(b)
}

// IDOf returns the id of the heartbeat a heartbeat event's Detail names (a
// miss's or a recovery's), 0 when it names none: an event recorded before
// details carried it, or one that is not a heartbeat's.
func IDOf(detail string) int64 {
	var d struct {
		HeartbeatID int64 `json:"heartbeat_id"`
	}
	if detail == "" || json.Unmarshal([]byte(detail), &d) != nil {
		return 0
	}
	return d.HeartbeatID
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
