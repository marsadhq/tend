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
// failure ping keeps its heartbeat.missed type and plain-name payload, but the
// subject the webhook posts says it failed, with the exit status, and its body
// carries the tail of the logged output. The envelope stays valid and its data
// unchanged.
func TestHeartbeatFailureSaysWhatHappened(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	three := 3
	ev := heartbeat.FailedEvent(1, 4, "db-backup", &three, "dumping...\npg_dump: error: connection refused\n")
	ev.ID, ev.CreatedAt = 41, at

	m := messageFor(ev, "primary", "")
	w := m.Webhook.(event.TendWebhook)
	if w.Subject != "Heartbeat failed: db-backup (exit 3)" {
		t.Fatalf("subject = %q", w.Subject)
	}
	if !strings.Contains(w.Body, "exit status 3") || !strings.HasSuffix(w.Body, "pg_dump: error: connection refused") {
		t.Fatalf("body = %q, want the exit status and the output tail", w.Body)
	}
	if w.Event == nil || w.Event.Type != "heartbeat.missed" || w.Event.Payload != "db-backup" {
		t.Fatalf("legacy event = %+v, want heartbeat.missed with the plain name", w.Event)
	}
	if w.Marsad == nil {
		t.Fatal("no envelope")
	}
	if err := w.Marsad.Validate(); err != nil {
		t.Fatalf("envelope invalid: %v", err)
	}
	if w.Marsad.Kind != "heartbeat.missed" || w.Marsad.Title != w.Subject || w.Marsad.Body != w.Body {
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
	want, err := json.Marshal(map[string]any{"subject": w.Subject, "body": w.Body, "event": ev})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("legacy body:\n got %s\nwant %s", got, want)
	}
}

// TestHeartbeatFailureWithoutExitStatusOrOutput: /fail with no body.
func TestHeartbeatFailureWithoutExitStatusOrOutput(t *testing.T) {
	w := messageFor(heartbeat.FailedEvent(1, 4, "sync", nil, ""), "primary", "").Webhook.(event.TendWebhook)
	if w.Subject != "Heartbeat failed: sync" {
		t.Fatalf("subject = %q", w.Subject)
	}
	if w.Body != "sync reported a failure" {
		t.Fatalf("body = %q", w.Body)
	}
}

// TestHeartbeatOverranSaysSo: a run that started and did not finish within
// its grace.
func TestHeartbeatOverranSaysSo(t *testing.T) {
	at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	w := messageFor(heartbeat.OverranEvent(heartbeat.Heartbeat{ID: 4, OrgID: 1, Name: "sync", GraceSeconds: 300, StartedAt: at}), "primary", "").Webhook.(event.TendWebhook)
	if w.Subject != "Heartbeat missed: sync (run did not finish)" {
		t.Fatalf("subject = %q", w.Subject)
	}
	if w.Body != "sync started a run at 2026-10-20T10:00:00Z that did not finish within 5m0s" {
		t.Fatalf("body = %q", w.Body)
	}
}

// TestPlainHeartbeatMissUnchanged: for a plain miss (the watcher's, with the
// last ping, period and grace in its detail, and every event stored before
// misses had one) the webhook's text is what it always was.
func TestPlainHeartbeatMissUnchanged(t *testing.T) {
	for _, detail := range []string{"", "junk", `{"reason":"something-new"}`,
		`{"reason":"missed","heartbeat_id":4,"last_seen_at":"2026-10-20T03:00:00Z","period_seconds":86400,"grace_seconds":3600}`} {
		w := messageFor(core.Event{Type: "heartbeat.missed", Payload: "db-backup", Detail: detail}, "primary", "").Webhook.(event.TendWebhook)
		if w.Subject != "Heartbeat missed: db-backup" || w.Body != "db-backup" {
			t.Errorf("detail %q: subject %q body %q, want the plain miss", detail, w.Subject, w.Body)
		}
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
