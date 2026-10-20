package notify

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/marsadhq/kit/event"
	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
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
// subject, the body and the originating event under "event"), with the event's
// marsad.event/v1 envelope added under "marsad" when it has one (see
// envelope). instance names this installation in the envelope's source.
func messageFor(ev core.Event, instance string) kitnotify.Message {
	subject, body := render(ev)
	env := envelope(ev, subject, body, instance)
	return kitnotify.Message{
		Subject: subject,
		Body:    body,
		Event:   env,
		Webhook: event.TendWebhook{Subject: subject, Body: body, Event: tendEvent(ev), Marsad: env},
	}
}

// envelopeKind is how one event type maps onto the V1 envelope.
type envelopeKind struct {
	subject  string // "job", "heartbeat" or "notifier"
	severity event.Severity
	status   event.Status
}

// envelopeKinds maps every alertable event type onto V1, as kit/event
// documents Tend's mapping (see event.TendFixture) and its golden fixtures
// pin it.
var envelopeKinds = map[string]envelopeKind{
	"run.failed":            {"job", event.SeverityCritical, event.StatusFiring},
	"run.recovered":         {"job", event.SeverityCritical, event.StatusResolved},
	"heartbeat.missed":      {"heartbeat", event.SeverityCritical, event.StatusFiring},
	"heartbeat.recovered":   {"heartbeat", event.SeverityCritical, event.StatusResolved},
	"job.removed":           {"job", event.SeverityInfo, event.StatusResolved},
	"heartbeat.removed":     {"heartbeat", event.SeverityInfo, event.StatusResolved},
	EventNotificationFailed: {"notifier", event.SeverityWarning, event.StatusFiring},
}

// envelope is ev as a marsad.event/v1 envelope: source {tend, instance}, the
// event's row id, its type as the kind, the job (from the payload's job_id and
// job_name), the heartbeat (its name, which is its identity) or Tend's
// notifier as the subject, the severity and status of envelopeKinds, the
// rendered subject and body as title and body, and CreatedAt as occurred_at.
// dedup_key is the event's DedupKey, or "<type>:<payload>" without one,
// followed by "@" and CreatedAt in unix nanoseconds, the identity Ward already
// derives from the legacy event; sequence is event.SequenceAt(CreatedAt).
//
// It is nil when ev cannot make a valid envelope (a type with no mapping, a
// run payload without a job_id, an unnamed heartbeat, no CreatedAt): the
// webhook then posts the legacy body alone, as it always has, rather than
// failing the delivery over the addition.
func envelope(ev core.Event, title, body, instance string) *event.V1 {
	k, ok := envelopeKinds[ev.Type]
	if !ok {
		return nil
	}
	var subject event.Subject
	var data []byte
	switch k.subject {
	case "job":
		var p struct {
			JobID   int64  `json:"job_id"`
			JobName string `json:"job_name"`
		}
		if json.Unmarshal([]byte(ev.Payload), &p) != nil || p.JobID <= 0 {
			return nil
		}
		subject = event.Subject{Type: "job", ID: strconv.FormatInt(p.JobID, 10), Name: p.JobName}
		data = []byte(ev.Payload)
	case "heartbeat":
		subject = event.Subject{Type: "heartbeat", ID: ev.Payload, Name: ev.Payload}
		data, _ = json.Marshal(map[string]string{"heartbeat": ev.Payload})
	case "notifier":
		subject = event.Subject{Type: "notifier", ID: "notify", Name: "notify"}
		data, _ = json.Marshal(map[string]string{"undelivered": ev.Payload})
	}
	base := ev.DedupKey
	if base == "" {
		base = ev.Type + ":" + ev.Payload
	}
	env := event.V1{
		Schema:     event.SchemaV1,
		ID:         strconv.FormatInt(ev.ID, 10),
		Source:     event.Source{Tool: "tend", Instance: instance},
		Kind:       ev.Type,
		Subject:    subject,
		Severity:   k.severity,
		Status:     k.status,
		DedupKey:   fmt.Sprintf("%s@%d", base, ev.CreatedAt.UnixNano()),
		Sequence:   event.SequenceAt(ev.CreatedAt),
		OccurredAt: ev.CreatedAt,
		Title:      title,
		Body:       body,
		Data:       json.RawMessage(data),
	}
	if env.Validate() != nil {
		return nil
	}
	return &env
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

// Render returns the subject and body a notification of ev carries, for
// showing an event elsewhere (the CLI's heartbeat history) the way an alert
// shows it.
func Render(ev core.Event) (subject, body string) { return render(ev) }

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
		return renderMissed(ev)

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

// renderMissed is the subject and body of a heartbeat.missed. A plain miss
// (no detail, or one this version does not know) reads as it always has; one
// caused by a failure ping or an overrunning run says so, and a failure's
// body ends with the tail of what the ping logged.
func renderMissed(ev core.Event) (string, string) {
	name := ev.Payload
	d, ok := heartbeat.ParseMissDetail(ev.Detail)
	switch {
	case ok && d.Reason == heartbeat.ReasonFailed:
		subject, body := "Heartbeat failed: "+name, name+" reported a failure"
		if d.ExitStatus != nil {
			subject += fmt.Sprintf(" (exit %d)", *d.ExitStatus)
			body += fmt.Sprintf(" (exit status %d)", *d.ExitStatus)
		}
		if d.Output != "" {
			body += "\n\nLast output:\n" + d.Output
		}
		return subject, body
	case ok && d.Reason == heartbeat.ReasonOverran:
		grace := time.Duration(d.GraceSeconds) * time.Second
		return "Heartbeat missed: " + name + " (run did not finish)",
			fmt.Sprintf("%s started a run at %s that did not finish within %s",
				name, d.StartedAt.UTC().Format(time.RFC3339), grace)
	default:
		return "Heartbeat missed: " + name, name
	}
}
