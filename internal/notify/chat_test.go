package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/event"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
)

// chatCase is one event and the subject and body a person should read.
type chatCase struct {
	name        string
	ev          core.Event
	subject     string
	body        string
	legacyTitle string // the webhook's subject, unchanged
	legacyBody  string // the webhook's body, unchanged
}

// The alert a person reads on a phone (Telegram, Slack, Discord, email, ntfy,
// Pushover) names the job or heartbeat once, in the subject, and says in the
// body what the subject does not: when a heartbeat last pinged and what it
// expects, the last lines of a failed run or ping, the timeout a run hit. The
// webhook keeps its text, byte for byte.
func TestChatTextSaysWhatHappenedOnce(t *testing.T) {
	at := time.Date(2026, 10, 21, 14, 10, 0, 0, time.UTC)
	three := 3
	failed := heartbeat.FailedEvent(1, 4, "db-backup", &three, "dumping...\npg_dump: error: connection refused\n")
	failed.CreatedAt = at
	quiet := heartbeat.OverdueEvent(heartbeat.Heartbeat{ID: 4, OrgID: 1, Name: "ward-live-test", Status: "up",
		LastSeenAt: at.Add(-25*time.Hour - time.Minute), PeriodSeconds: 86400, GraceSeconds: 3600})
	quiet.CreatedAt = at
	never := heartbeat.OverdueEvent(heartbeat.Heartbeat{ID: 4, OrgID: 1, Name: "cron", Status: "new", PeriodSeconds: 60})
	never.CreatedAt = at
	stuck := heartbeat.OverranEvent(heartbeat.Heartbeat{ID: 4, OrgID: 1, Name: "sync", PeriodSeconds: 3600,
		GraceSeconds: 300, StartedAt: at.Add(-5 * time.Minute)})
	stuck.CreatedAt = at

	for _, tc := range []chatCase{
		{name: "plain miss from before misses had a detail",
			ev:      core.Event{Type: "heartbeat.missed", Payload: "ward-live-test", CreatedAt: at},
			subject: "Heartbeat missed: ward-live-test", body: "",
			legacyTitle: "Heartbeat missed: ward-live-test", legacyBody: "ward-live-test"},
		{name: "plain miss", ev: quiet,
			subject:     "Heartbeat missed: ward-live-test",
			body:        "No ping for 1d 1h, since 2026-10-20 13:09 UTC; one is expected every 1d, with 1h of grace.",
			legacyTitle: "Heartbeat missed: ward-live-test", legacyBody: "ward-live-test"},
		{name: "never pinged", ev: never,
			subject:     "Heartbeat missed: cron",
			body:        "It has never pinged; one is expected every 1m, with no grace.",
			legacyTitle: "Heartbeat missed: cron", legacyBody: "cron"},
		{name: "overran", ev: stuck,
			subject:     "Heartbeat missed: sync (run did not finish)",
			body:        "A run started at 2026-10-21 14:05 UTC and did not finish within 5m.",
			legacyTitle: "Heartbeat missed: sync (run did not finish)",
			legacyBody:  "sync started a run at 2026-10-21T14:05:00Z that did not finish within 5m0s"},
		{name: "failure ping", ev: failed,
			subject:     "Heartbeat failed: db-backup (exit 3)",
			body:        "Last output:\ndumping...\npg_dump: error: connection refused",
			legacyTitle: "Heartbeat failed: db-backup (exit 3)",
			legacyBody:  "db-backup reported a failure (exit status 3)\n\nLast output:\ndumping...\npg_dump: error: connection refused"},
		{name: "recovered",
			ev:      core.Event{Type: "heartbeat.recovered", Payload: "db-backup", CreatedAt: at},
			subject: "Heartbeat recovered: db-backup", body: "",
			legacyTitle: "Heartbeat recovered: db-backup", legacyBody: "db-backup"},
		{name: "run failed",
			ev: core.Event{Type: "run.failed", CreatedAt: at,
				Payload: `{"run_id":17,"job_id":2,"job_name":"nightly","status":"failed","exit_code":3}`,
				Detail:  `{"output":"uploading chunk 40/40\nUploadError: chunk 41 failed"}`},
			subject:     "Job failed: nightly (exit 3)",
			body:        "Last output:\nuploading chunk 40/40\nUploadError: chunk 41 failed",
			legacyTitle: "Job failed: nightly", legacyBody: "status=failed exit_code=3"},
		{name: "run failed without an exit code or output",
			ev: core.Event{Type: "run.failed", CreatedAt: at,
				Payload: `{"run_id":17,"job_id":2,"job_name":"web-cert","status":"failed","exit_code":-1}`},
			subject: "Job failed: web-cert", body: "",
			legacyTitle: "Job failed: web-cert", legacyBody: "status=failed exit_code=-1"},
		{name: "run timed out",
			ev: core.Event{Type: "run.failed", CreatedAt: at,
				Payload: `{"run_id":18,"job_id":3,"job_name":"report","status":"timed_out","exit_code":-1}`,
				Detail:  `{"output":"querying warehouse...","timeout_seconds":1800}`},
			subject:     "Job timed out: report",
			body:        "It was stopped after its 30m timeout.\n\nLast output:\nquerying warehouse...",
			legacyTitle: "Job failed: report", legacyBody: "status=timed_out exit_code=-1"},
		{name: "run recovered",
			ev: core.Event{Type: "run.recovered", CreatedAt: at,
				Payload: `{"run_id":19,"job_id":2,"job_name":"nightly","status":"succeeded","exit_code":0}`},
			subject:     "Job recovered: nightly",
			body:        "Its previous run had not succeeded.",
			legacyTitle: "Job recovered: nightly", legacyBody: "status=succeeded exit_code=0; the previous run had not succeeded"},
	} {
		m := messageFor(tc.ev, "primary")
		if m.Subject != tc.subject || m.Body != tc.body {
			t.Errorf("%s:\n got subject %q body %q\nwant subject %q body %q", tc.name, m.Subject, m.Body, tc.subject, tc.body)
		}
		w := m.Webhook.(event.TendWebhook)
		if w.Subject != tc.legacyTitle || w.Body != tc.legacyBody {
			t.Errorf("%s: the webhook's text changed: subject %q body %q", tc.name, w.Subject, w.Body)
		}
		if w.Marsad != nil && (w.Marsad.Title != tc.legacyTitle || w.Marsad.Body != tc.legacyBody) {
			t.Errorf("%s: the envelope's text changed: title %q body %q", tc.name, w.Marsad.Title, w.Marsad.Body)
		}
		// A chat channel shows the subject, a newline and the body: no line
		// may just repeat the name the subject already gave.
		name := tc.ev.Payload
		if strings.HasPrefix(name, "{") {
			name = ""
		}
		for _, line := range strings.Split(m.Body, "\n") {
			if name != "" && line == name {
				t.Errorf("%s: body line %q repeats the name", tc.name, line)
			}
		}
	}
}

// The events that already read well keep their text in chat too.
func TestChatTextKeepsTheRest(t *testing.T) {
	for _, ev := range []core.Event{
		{Type: "job.removed", Payload: `{"job_id":7,"job_name":"nightly"}`},
		{Type: "heartbeat.removed", Payload: "db-backup"},
		{Type: "notification.failed", Payload: "run.failed"},
		{Type: "weird.event", Payload: "x"},
	} {
		m := messageFor(ev, "primary")
		w := m.Webhook.(event.TendWebhook)
		if m.Subject != w.Subject || m.Body != w.Body {
			t.Errorf("%s: chat %q / %q, webhook %q / %q; want the same", ev.Type, m.Subject, m.Body, w.Subject, w.Body)
		}
	}
}

func TestSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                               "0s",
		45 * time.Second:                "45s",
		90 * time.Second:                "1m 30s",
		time.Hour + 30*time.Second:      "1h",
		25*time.Hour + time.Minute:      "1d 1h",
		24*time.Hour + 5*time.Minute:    "1d",
		7 * 24 * time.Hour:              "7d",
		26*time.Hour + 59*time.Minute:   "1d 2h",
		59*time.Minute + 59*time.Second: "59m 59s",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q, want %q", d, got, want)
		}
	}
}
