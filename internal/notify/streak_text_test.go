package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/event"
	"github.com/marsadhq/tend/internal/core"
)

// A reminder says the job is still failing, how many runs in a row have
// failed and since when; the first failure of a streak reads as it always
// has; a recovery says what it ended. The webhook's text and the envelope do
// not change.
func TestChatTextOfAFailingStreak(t *testing.T) {
	since := `"failing_since":"2026-10-28T19:04:00Z"`
	at := time.Date(2026, 10, 28, 22, 4, 0, 0, time.UTC)
	for _, tc := range []chatCase{
		{name: "the first failure",
			ev: core.Event{Type: "run.failed", CreatedAt: time.Date(2026, 10, 28, 19, 4, 0, 0, time.UTC),
				Payload: `{"run_id":17,"job_id":2,"job_name":"api-health","status":"failed","exit_code":503}`,
				Detail:  `{"http_status":503,"failed_runs":1,` + since + `}`},
			subject: "Job failed: api-health (HTTP 503)", body: "",
			legacyTitle: "Job failed: api-health", legacyBody: "status=failed exit_code=503"},
		{name: "a reminder",
			ev: core.Event{Type: "run.failed", CreatedAt: at,
				Payload: `{"run_id":53,"job_id":2,"job_name":"api-health","status":"failed","exit_code":503}`,
				Detail:  `{"http_status":503,"failed_runs":37,` + since + `}`},
			subject:     "Job still failing: api-health (HTTP 503)",
			body:        "37 runs in a row have failed, since 2026-10-28 19:04 UTC (3h).",
			legacyTitle: "Job failed: api-health", legacyBody: "status=failed exit_code=503"},
		{name: "a reminder with an exit code and output",
			ev: core.Event{Type: "run.failed", CreatedAt: at.Add(25 * time.Minute),
				Payload: `{"run_id":54,"job_id":3,"job_name":"nightly","status":"failed","exit_code":3}`,
				Detail:  `{"output":"UploadError: chunk 41","failed_runs":2,` + since + `}`},
			subject:     "Job still failing: nightly (exit 3)",
			body:        "2 runs in a row have failed, since 2026-10-28 19:04 UTC (3h 25m).\n\nLast output:\nUploadError: chunk 41",
			legacyTitle: "Job failed: nightly", legacyBody: "status=failed exit_code=3"},
		{name: "a reminder that timed out",
			ev: core.Event{Type: "run.failed", CreatedAt: at,
				Payload: `{"run_id":55,"job_id":4,"job_name":"report","status":"timed_out","exit_code":-1}`,
				Detail:  `{"timeout_seconds":1800,"failed_runs":4,` + since + `}`},
			subject:     "Job still failing: report (timed out)",
			body:        "4 runs in a row have failed, since 2026-10-28 19:04 UTC (3h). It was stopped after its 30m timeout.",
			legacyTitle: "Job failed: report", legacyBody: "status=timed_out exit_code=-1"},
		{name: "a recovery from a streak",
			ev: core.Event{Type: "run.recovered", CreatedAt: time.Date(2026, 10, 29, 13, 4, 0, 0, time.UTC),
				Payload: `{"run_id":233,"job_id":2,"job_name":"api-health","status":"succeeded","exit_code":0}`,
				Detail:  `{"failed_runs":216,` + since + `}`},
			subject:     "Job recovered: api-health",
			body:        "It succeeded after 216 failed runs in a row, since 2026-10-28 19:04 UTC (18h).",
			legacyTitle: "Job recovered: api-health", legacyBody: "status=succeeded exit_code=0; the previous run had not succeeded"},
		{name: "a recovery from one failure",
			ev: core.Event{Type: "run.recovered", CreatedAt: at,
				Payload: `{"run_id":20,"job_id":2,"job_name":"api-health","status":"succeeded","exit_code":0}`,
				Detail:  `{"failed_runs":1,` + since + `}`},
			subject: "Job recovered: api-health", body: "Its previous run had not succeeded.",
			legacyTitle: "Job recovered: api-health", legacyBody: "status=succeeded exit_code=0; the previous run had not succeeded"},
	} {
		m := messageFor(tc.ev, "primary", chat10)
		if m.Subject != tc.subject || plain(m) != tc.body {
			t.Errorf("%s:\n got subject %q body %q\nwant subject %q body %q", tc.name, m.Subject, plain(m), tc.subject, tc.body)
		}
		w := m.Webhook.(event.TendWebhook)
		if w.Subject != tc.legacyTitle || w.Body != tc.legacyBody {
			t.Errorf("%s: the webhook's text changed: subject %q body %q", tc.name, w.Subject, w.Body)
		}
		if w.Marsad == nil || w.Marsad.Title != tc.legacyTitle || w.Marsad.Body != tc.legacyBody {
			t.Errorf("%s: the envelope changed: %+v", tc.name, w.Marsad)
		}
		if w.Event == nil || strings.Contains(w.Event.Payload, "failed_runs") {
			t.Errorf("%s: the webhook's event carries the streak", tc.name)
		}
	}
}
