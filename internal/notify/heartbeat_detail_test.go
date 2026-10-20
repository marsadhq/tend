package notify

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/event"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
)

// TestHeartbeatFailureSaysWhatHappened: a heartbeat that went down on a
// failure ping keeps its heartbeat.missed type and plain-name payload, but its
// subject says it failed, with the exit status, and its body carries the tail
// of the logged output. The envelope stays valid and its data unchanged.
func TestHeartbeatFailureSaysWhatHappened(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	three := 3
	ev := heartbeat.FailedEvent(1, "db-backup", &three, "dumping...\npg_dump: error: connection refused\n")
	ev.ID, ev.CreatedAt = 41, at

	m := messageFor(ev, "primary")
	if m.Subject != "Heartbeat failed: db-backup (exit 3)" {
		t.Fatalf("subject = %q", m.Subject)
	}
	if !strings.Contains(m.Body, "exit status 3") || !strings.HasSuffix(m.Body, "pg_dump: error: connection refused") {
		t.Fatalf("body = %q, want the exit status and the output tail", m.Body)
	}
	w := m.Webhook.(event.TendWebhook)
	if w.Event == nil || w.Event.Type != "heartbeat.missed" || w.Event.Payload != "db-backup" {
		t.Fatalf("legacy event = %+v, want heartbeat.missed with the plain name", w.Event)
	}
	if w.Marsad == nil {
		t.Fatal("no envelope")
	}
	if err := w.Marsad.Validate(); err != nil {
		t.Fatalf("envelope invalid: %v", err)
	}
	if w.Marsad.Kind != "heartbeat.missed" || w.Marsad.Title != m.Subject || w.Marsad.Body != m.Body {
		t.Fatalf("envelope = %+v, want kind heartbeat.missed carrying the subject and body", w.Marsad)
	}
	if string(w.Marsad.Data) != `{"heartbeat":"db-backup"}` || w.Marsad.DedupKey != "heartbeat.missed:db-backup@"+itoa(at.UnixNano()) {
		t.Fatalf("envelope data %s / dedup %q changed", w.Marsad.Data, w.Marsad.DedupKey)
	}

	// The body a webhook receives is still exactly the legacy encoding
	// around the stored event, which does not carry the detail.
	got, err := json.Marshal(event.TendWebhook{Subject: w.Subject, Body: w.Body, Event: w.Event})
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(map[string]any{"subject": m.Subject, "body": m.Body, "event": ev})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("legacy body:\n got %s\nwant %s", got, want)
	}
}

// TestHeartbeatFailureWithoutExitStatusOrOutput: /fail with no body.
func TestHeartbeatFailureWithoutExitStatusOrOutput(t *testing.T) {
	m := messageFor(heartbeat.FailedEvent(1, "sync", nil, ""), "primary")
	if m.Subject != "Heartbeat failed: sync" {
		t.Fatalf("subject = %q", m.Subject)
	}
	if m.Body != "sync reported a failure" {
		t.Fatalf("body = %q", m.Body)
	}
}

// TestHeartbeatOverranSaysSo: a run that started and did not finish within
// its grace.
func TestHeartbeatOverranSaysSo(t *testing.T) {
	at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	m := messageFor(heartbeat.OverranEvent(1, "sync", at, 300), "primary")
	if m.Subject != "Heartbeat missed: sync (run did not finish)" {
		t.Fatalf("subject = %q", m.Subject)
	}
	if m.Body != "sync started a run at 2026-10-20T10:00:00Z that did not finish within its grace of 5m0s" {
		t.Fatalf("body = %q", m.Body)
	}
}

// TestPlainHeartbeatMissUnchanged: without a detail (the watcher's miss, and
// every event stored before details existed) the text is what it always was.
func TestPlainHeartbeatMissUnchanged(t *testing.T) {
	for _, detail := range []string{"", "junk", `{"reason":"something-new"}`} {
		m := messageFor(core.Event{Type: "heartbeat.missed", Payload: "db-backup", Detail: detail}, "primary")
		if m.Subject != "Heartbeat missed: db-backup" || m.Body != "db-backup" {
			t.Errorf("detail %q: subject %q body %q, want the plain miss", detail, m.Subject, m.Body)
		}
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
