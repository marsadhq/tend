package notify

import (
	"strings"
	"testing"

	"github.com/marsadhq/kit/event"
	"github.com/marsadhq/tend/internal/core"
)

// TestMessageForRunFailed asserts the run.failed message carries the job name in
// the subject, and the webhook's body the status and exit_code.
func TestMessageForRunFailed(t *testing.T) {
	ev := core.Event{
		OrgID:   1,
		Type:    "run.failed",
		Payload: `{"run_id":3,"job_id":2,"job_name":"nightly","status":"failed","exit_code":9}`,
	}
	m := messageFor(ev, "primary", chat10)
	if w, ok := m.Webhook.(event.TendWebhook); !ok || w.Event == nil || w.Event.Type != "run.failed" {
		t.Fatalf("Event not carried through to the webhook body: %+v", m.Webhook)
	}
	if !strings.Contains(m.Subject, "nightly") {
		t.Fatalf("subject should mention job name: %q", m.Subject)
	}
	if w := m.Webhook.(event.TendWebhook); w.Body != "status=failed exit_code=9" {
		t.Fatalf("webhook body should give status and exit_code: %q", w.Body)
	}

	// A run.failed with an unparseable payload must not panic and must still
	// produce a sensible subject.
	m2 := messageFor(core.Event{Type: "run.failed", Payload: "not json"}, "primary", chat10)
	if m2.Subject == "" {
		t.Fatal("empty subject for unparseable run.failed payload")
	}
}

// TestMessageForHeartbeat asserts heartbeat messages include the heartbeat name
// (carried as the plain-string payload).
func TestMessageForHeartbeat(t *testing.T) {
	missed := messageFor(core.Event{Type: "heartbeat.missed", Payload: "db-backup"}, "primary", chat10)
	if !strings.Contains(missed.Subject, "db-backup") {
		t.Fatalf("missed subject should mention heartbeat name: %q", missed.Subject)
	}
	recovered := messageFor(core.Event{Type: "heartbeat.recovered", Payload: "db-backup"}, "primary", chat10)
	if !strings.Contains(recovered.Subject, "db-backup") {
		t.Fatalf("recovered subject should mention heartbeat name: %q", recovered.Subject)
	}

	// Unknown type falls back to the type as subject.
	other := messageFor(core.Event{Type: "weird.event", Payload: "x"}, "primary", chat10)
	if other.Subject != "weird.event" {
		t.Fatalf("default subject: got %q want %q", other.Subject, "weird.event")
	}
}

// TestEventJobIDPayloadShapes covers the payload-parsing helper across the
// shapes it actually sees: run.failed JSON, plain heartbeat name, empty, junk.
func TestEventJobIDPayloadShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    int64
	}{
		{"run.failed JSON", `{"run_id":1,"job_id":7,"job_name":"x","status":"failed","exit_code":2}`, 7},
		{"plain name", "nightly-backup", 0},
		{"empty", "", 0},
		{"malformed JSON", `{"job_id":`, 0},
		{"json without job_id", `{"run_id":1}`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EventJobID(core.Event{Payload: c.payload}); got != c.want {
				t.Fatalf("EventJobID(%q): got %d want %d", c.payload, got, c.want)
			}
		})
	}
}

// TestMessageForRunRecovered asserts run.recovered names the job in the
// subject and is alertable, unlike run.succeeded.
func TestMessageForRunRecovered(t *testing.T) {
	m := messageFor(core.Event{
		Type:    "run.recovered",
		Payload: `{"run_id":4,"job_id":2,"job_name":"nightly","status":"succeeded","exit_code":0}`,
	}, "primary", chat10)
	if m.Subject != "Job recovered: nightly" {
		t.Fatalf("subject = %q, want %q", m.Subject, "Job recovered: nightly")
	}
	if !strings.Contains(m.Body, "succeeded") {
		t.Fatalf("body should mention the status: %q", m.Body)
	}
	if m2 := messageFor(core.Event{Type: "run.recovered", Payload: "junk"}, "primary", chat10); m2.Subject != "Job recovered" {
		t.Fatalf("unparseable payload subject = %q, want %q", m2.Subject, "Job recovered")
	}
	if !Alertable("run.recovered") || Alertable("run.succeeded") {
		t.Fatal("run.recovered must be alertable and run.succeeded must not")
	}
}

// TestMessageForRemovals asserts job.removed and heartbeat.removed name what
// was deleted and are alertable.
func TestMessageForRemovals(t *testing.T) {
	job := messageFor(core.Event{Type: "job.removed", Payload: `{"job_id":7,"job_name":"nightly"}`}, "primary", chat10)
	if job.Subject != "Job removed: nightly" {
		t.Fatalf("job.removed subject = %q", job.Subject)
	}
	hb := messageFor(core.Event{Type: "heartbeat.removed", Payload: "db-backup"}, "primary", chat10)
	if hb.Subject != "Heartbeat removed: db-backup" {
		t.Fatalf("heartbeat.removed subject = %q", hb.Subject)
	}
	for _, typ := range []string{"job.removed", "heartbeat.removed"} {
		if !Alertable(typ) {
			t.Errorf("Alertable(%q) = false, want true", typ)
		}
	}
	if EventJobID(core.Event{Payload: `{"job_id":7,"job_name":"nightly"}`}) != 7 {
		t.Fatal("job.removed payload must scope to its job")
	}
}

// TestMessageForNotificationFailed asserts the failure report names the event
// type that was not delivered.
func TestMessageForNotificationFailed(t *testing.T) {
	m := messageFor(core.Event{Type: "notification.failed", Payload: "run.failed"}, "primary", chat10)
	if m.Subject != "Alert delivery failed: run.failed" {
		t.Fatalf("subject = %q", m.Subject)
	}
	if !strings.Contains(m.Body, "run.failed") {
		t.Fatalf("body should name the undelivered event type: %q", m.Body)
	}
	if m2 := messageFor(core.Event{Type: "notification.failed"}, "primary", chat10); m2.Subject != "Alert delivery failed" {
		t.Fatalf("empty payload subject = %q", m2.Subject)
	}
}

// TestValidateRuleEvent pins which event types a rule may name.
func TestValidateRuleEvent(t *testing.T) {
	for _, ev := range AlertableTypes() {
		if err := ValidateRuleEvent(ev); err != nil {
			t.Errorf("ValidateRuleEvent(%q) = %v, want nil", ev, err)
		}
	}
	for _, ev := range []string{"run.succeeded", "run.started", "", "RUN.FAILED", "notification.sent"} {
		err := ValidateRuleEvent(ev)
		if err == nil {
			t.Errorf("ValidateRuleEvent(%q) = nil, want an error", ev)
			continue
		}
		if !strings.Contains(err.Error(), "heartbeat.missed") {
			t.Errorf("ValidateRuleEvent(%q) error %q does not list the valid events", ev, err)
		}
	}
	if got := len(AlertableTypes()); got != 7 {
		t.Errorf("AlertableTypes() has %d entries, want 7", got)
	}
}
