package notify

import (
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/core"
)

// TestMessageForRunFailed asserts the run.failed message carries the job name in
// the subject and status/exit_code in the body.
func TestMessageForRunFailed(t *testing.T) {
	ev := core.Event{
		OrgID:   1,
		Type:    "run.failed",
		Payload: `{"run_id":3,"job_id":2,"job_name":"nightly","status":"failed","exit_code":9}`,
	}
	m := messageFor(ev)
	if m.Event.Type != "run.failed" {
		t.Fatalf("Event not carried through: %+v", m.Event)
	}
	if !strings.Contains(m.Subject, "nightly") {
		t.Fatalf("subject should mention job name: %q", m.Subject)
	}
	if !strings.Contains(m.Body, "failed") || !strings.Contains(m.Body, "9") {
		t.Fatalf("body should mention status and exit_code: %q", m.Body)
	}

	// A run.failed with an unparseable payload must not panic and must still
	// produce a sensible subject.
	m2 := messageFor(core.Event{Type: "run.failed", Payload: "not json"})
	if m2.Subject == "" {
		t.Fatal("empty subject for unparseable run.failed payload")
	}
}

// TestMessageForHeartbeat asserts heartbeat messages include the heartbeat name
// (carried as the plain-string payload).
func TestMessageForHeartbeat(t *testing.T) {
	missed := messageFor(core.Event{Type: "heartbeat.missed", Payload: "db-backup"})
	if !strings.Contains(missed.Subject, "db-backup") {
		t.Fatalf("missed subject should mention heartbeat name: %q", missed.Subject)
	}
	recovered := messageFor(core.Event{Type: "heartbeat.recovered", Payload: "db-backup"})
	if !strings.Contains(recovered.Subject, "db-backup") {
		t.Fatalf("recovered subject should mention heartbeat name: %q", recovered.Subject)
	}

	// Unknown type falls back to the type as subject.
	other := messageFor(core.Event{Type: "weird.event", Payload: "x"})
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
