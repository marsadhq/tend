package notify

import (
	"encoding/json"
	"fmt"

	"github.com/marsadhq/tend/internal/core"
)

// alertable is the set of event types that may trigger notifications.
// notification.* is deliberately excluded so a delivery failure (which emits
// notification.failed) can never enqueue a new delivery and start a loop.
// Non-alert lifecycle events (run.started, run.succeeded) are likewise absent,
// so emitters can fire on every event and let this filter drop the
// non-failures.
var alertable = map[string]bool{
	"run.failed":          true, // timeouts surface as run.failed (status in payload)
	"heartbeat.missed":    true,
	"heartbeat.recovered": true,
}

// Alertable reports whether events of type t may trigger notifications. The
// store consults it when enqueueing deliveries transactionally with an event
// insert; it is the loop guard that keeps notification.* events from ever
// producing deliveries of their own.
func Alertable(t string) bool { return alertable[t] }

// EventJobID extracts the job_id from a run.* event's JSON payload, or 0 when
// the payload is absent, not JSON, or carries no job_id (e.g. heartbeat events
// whose payload is a plain name). A 0 job_id matches org-wide rules only. The
// store uses it to scope rule matching when enqueueing deliveries.
func EventJobID(ev core.Event) int64 {
	if ev.Payload == "" {
		return 0
	}
	var p struct {
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
		return 0
	}
	return p.JobID
}

// messageFor renders a Subject/Body for ev, tailored per event type and robust
// to odd payloads (no panic). The originating event is always carried on
// Message.Event for providers that want structured access.
func messageFor(ev core.Event) Message {
	switch ev.Type {
	case "run.failed":
		var p struct {
			JobName  string `json:"job_name"`
			Status   string `json:"status"`
			ExitCode int    `json:"exit_code"`
		}
		// Best-effort: an unparseable payload just yields the generic subject.
		_ = json.Unmarshal([]byte(ev.Payload), &p)
		subject := "Job failed"
		if p.JobName != "" {
			subject = "Job failed: " + p.JobName
		}
		status := p.Status
		if status == "" {
			status = "failed"
		}
		return Message{
			Subject: subject,
			Body:    fmt.Sprintf("status=%s exit_code=%d", status, p.ExitCode),
			Event:   ev,
		}

	case "heartbeat.missed":
		return Message{
			Subject: "Heartbeat missed: " + ev.Payload,
			Body:    ev.Payload,
			Event:   ev,
		}

	case "heartbeat.recovered":
		return Message{
			Subject: "Heartbeat recovered: " + ev.Payload,
			Body:    ev.Payload,
			Event:   ev,
		}

	default:
		return Message{Subject: ev.Type, Body: ev.Payload, Event: ev}
	}
}
