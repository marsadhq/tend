package notify

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marsadhq/kit/event"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/core"
)

// alertableTypes lists, in the order they are documented, the event types
// that may trigger notifications. Non-alert lifecycle events (run.started,
// run.succeeded) are absent, so emitters can fire on every event and let this
// filter drop them. notification.failed is alertable so that a channel that
// stopped working can be reported through another one; the worker's loop
// guard keeps it from feeding itself: giving up on the delivery of a
// notification.failed records nothing further.
var alertableTypes = []string{
	"run.failed",    // timeouts surface as run.failed (status in payload)
	"run.recovered", // a success after a run that did not succeed
	"heartbeat.missed",
	"heartbeat.recovered",
	"job.removed",
	"heartbeat.removed",
	EventNotificationFailed, // see the loop guard in Worker.deliverOne
}

// alertable is alertableTypes as a set.
var alertable = func() map[string]bool {
	m := make(map[string]bool, len(alertableTypes))
	for _, t := range alertableTypes {
		m[t] = true
	}
	return m
}()

// Alertable reports whether events of type t may trigger notifications. The
// store consults it when enqueueing deliveries transactionally with an event
// insert.
func Alertable(t string) bool { return alertable[t] }

// AlertableTypes returns the event types a rule can route, in documentation
// order. The slice is a copy.
func AlertableTypes() []string { return append([]string(nil), alertableTypes...) }

// ValidateRuleEvent returns an error unless a notification rule for event
// type t can ever fire. Only alertable events are delivered, so a rule for
// any other type (run.succeeded, a typo) would be accepted and then do
// nothing at all; the error names the types that do work.
func ValidateRuleEvent(t string) error {
	if Alertable(t) {
		return nil
	}
	valid := strings.Join(alertableTypes, ", ")
	switch t {
	case "run.succeeded", "run.started":
		return fmt.Errorf("event %q never triggers a notification (use run.recovered to hear when a failing job succeeds again); valid events: %s", t, valid)
	default:
		return fmt.Errorf("unknown event %q; valid events: %s", t, valid)
	}
}

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

// messageFor renders the Message for ev: a subject and body tailored per event
// type, and the webhook body Tend has always posted (event.TendWebhook: the
// subject, the body and the originating event under "event").
func messageFor(ev core.Event) kitnotify.Message {
	subject, body := render(ev)
	return kitnotify.Message{
		Subject: subject,
		Body:    body,
		Webhook: event.TendWebhook{Subject: subject, Body: body, Event: tendEvent(ev)},
	}
}

// tendEvent is ev as the webhook body carries it.
func tendEvent(ev core.Event) *event.TendEvent {
	return &event.TendEvent{
		ID:        ev.ID,
		OrgID:     ev.OrgID,
		Type:      ev.Type,
		Source:    ev.Source,
		Payload:   ev.Payload,
		DedupKey:  ev.DedupKey,
		CreatedAt: ev.CreatedAt,
	}
}

// render returns the subject and body for ev, tailored per event type and
// robust to odd payloads (no panic).
func render(ev core.Event) (string, string) {
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
		return subject, fmt.Sprintf("status=%s exit_code=%d", status, p.ExitCode)

	case "run.recovered":
		var p struct {
			JobName  string `json:"job_name"`
			Status   string `json:"status"`
			ExitCode int    `json:"exit_code"`
		}
		_ = json.Unmarshal([]byte(ev.Payload), &p)
		subject := "Job recovered"
		if p.JobName != "" {
			subject = "Job recovered: " + p.JobName
		}
		status := p.Status
		if status == "" {
			status = "succeeded"
		}
		return subject, fmt.Sprintf("status=%s exit_code=%d; the previous run had not succeeded", status, p.ExitCode)

	case "heartbeat.missed":
		return "Heartbeat missed: " + ev.Payload, ev.Payload

	case "heartbeat.recovered":
		return "Heartbeat recovered: " + ev.Payload, ev.Payload

	case "job.removed":
		var p struct {
			JobID   int64  `json:"job_id"`
			JobName string `json:"job_name"`
		}
		_ = json.Unmarshal([]byte(ev.Payload), &p)
		subject := "Job removed"
		if p.JobName != "" {
			subject = "Job removed: " + p.JobName
		}
		return subject, "the job was deleted; it will not run or alert again"

	case "heartbeat.removed":
		return "Heartbeat removed: " + ev.Payload, "the heartbeat was deleted; it is no longer monitored"

	case EventNotificationFailed:
		subject, what := "Alert delivery failed", "a notification"
		if ev.Payload != "" {
			subject, what = subject+": "+ev.Payload, "a "+ev.Payload+" notification"
		}
		return subject, what + " could not be delivered and was given up after retrying; that alert was not received"

	default:
		return ev.Type, ev.Payload
	}
}
