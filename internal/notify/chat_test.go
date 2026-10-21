package notify

import (
	"encoding/json"
	"fmt"
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
		m := messageFor(tc.ev, "primary", chat10)
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
		m := messageFor(ev, "primary", chat10)
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

// With Tend's external URL set, an alert links to the page that says more:
// the run, or the heartbeat, after what happened and before any output, so a
// channel that cuts long messages cuts the output and keeps the link. Without
// it, or for an event with no page to link to, there is no link line.
func TestChatTextLinksToThePage(t *testing.T) {
	at := time.Date(2026, 10, 21, 14, 10, 0, 0, time.UTC)
	quiet := heartbeat.OverdueEvent(heartbeat.Heartbeat{ID: 4, OrgID: 1, Name: "db-backup", Status: "up",
		LastSeenAt: at.Add(-2 * time.Hour), PeriodSeconds: 3600})
	quiet.CreatedAt = at
	failed := core.Event{Type: "run.failed", CreatedAt: at,
		Payload: `{"run_id":17,"job_id":2,"job_name":"nightly","status":"failed","exit_code":3}`,
		Detail:  `{"output":"boom"}`}
	recovered := core.Event{Type: "run.recovered", CreatedAt: at,
		Payload: `{"run_id":18,"job_id":2,"job_name":"nightly","status":"succeeded","exit_code":0}`}
	const base = "https://tend.example/ops"
	for _, tc := range []struct {
		name string
		ev   core.Event
		base string
		want string
	}{
		{"missed heartbeat", quiet, base,
			"No ping for 2h, since 2026-10-21 12:10 UTC; one is expected every 1h, with no grace.\n\nhttps://tend.example/ops/heartbeats/4"},
		{"failed run", failed, base, "https://tend.example/ops/runs/17\n\nLast output:\nboom"},
		{"recovered run", recovered, base, "Its previous run had not succeeded.\n\nhttps://tend.example/ops/runs/18"},
		{"no base URL", failed, "", "Last output:\nboom"},
		{"timed out run", core.Event{Type: "run.failed", Payload: `{"run_id":20,"job_id":3,"job_name":"report","status":"timed_out","exit_code":-1}`,
			Detail: `{"output":"querying...","timeout_seconds":60}`}, base,
			"It was stopped after its 1m timeout.\n\nhttps://tend.example/ops/runs/20\n\nLast output:\nquerying..."},
		{"failure ping", heartbeat.FailedEvent(1, 4, "db-backup", nil, "boom\n"), base,
			"https://tend.example/ops/heartbeats/4\n\nLast output:\nboom"},
		{"failed run without output", core.Event{Type: "run.failed", Payload: `{"run_id":19,"job_id":2,"job_name":"x","status":"failed","exit_code":-1}`}, base,
			"https://tend.example/ops/runs/19"},
		{"miss recorded without a detail", core.Event{Type: "heartbeat.missed", Payload: "old"}, base, ""},
		{"recovered heartbeat", core.Event{Type: "heartbeat.recovered", Payload: "db-backup"}, base, ""},
		{"removed job", core.Event{Type: "job.removed", Payload: `{"job_id":7,"job_name":"nightly"}`}, base,
			"the job was deleted; it will not run or alert again"},
	} {
		m := messageFor(tc.ev, "primary", ChatOptions{BaseURL: tc.base, OutputLines: 10})
		if m.Body != tc.want {
			t.Errorf("%s: body %q, want %q", tc.name, m.Body, tc.want)
		}
		if w := m.Webhook.(event.TendWebhook); strings.Contains(w.Body, "https://") || (w.Marsad != nil && strings.Contains(w.Marsad.Body, "https://")) {
			t.Errorf("%s: the link reached the webhook's text: %q", tc.name, w.Body)
		}
	}
}

// chat10 is the default chat text: ten lines of output, no links.
var chat10 = ChatOptions{OutputLines: 10}

// TEND_ALERT_OUTPUT_LINES sets how many lines of output a chat alert shows:
// fewer or more than the default ten (an event keeps up to fifty, within
// 1000 bytes), or none at all, while the rest of the alert, its link
// included, stays. The webhook's text keeps its ten lines whatever it is.
func TestChatTextOutputLines(t *testing.T) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	output := strings.Join(lines, "\n")
	failedPing := heartbeat.FailedEvent(1, 4, "db-backup", nil, output)
	timedOut := core.Event{Type: "run.failed",
		Payload: `{"run_id":20,"job_id":3,"job_name":"report","status":"timed_out","exit_code":-1}`}
	detail, _ := json.Marshal(map[string]any{"output": core.TailLines(output, core.MaxOutputTailLines), "timeout_seconds": 60})
	timedOut.Detail = string(detail)
	const base = "https://tend.example"

	for _, tc := range []struct {
		name  string
		ev    core.Event
		lines int
		want  string
	}{
		{"failure ping, none", failedPing, 0, base + "/heartbeats/4"},
		{"failure ping, three", failedPing, 3, base + "/heartbeats/4\n\nLast output:\nline 28\nline 29\nline 30"},
		{"timed out run, none", timedOut, 0, "It was stopped after its 1m timeout.\n\n" + base + "/runs/20"},
		{"timed out run, thirty", timedOut, 30, "It was stopped after its 1m timeout.\n\n" + base + "/runs/20\n\nLast output:\n" + output},
	} {
		m := messageFor(tc.ev, "primary", ChatOptions{BaseURL: base, OutputLines: tc.lines})
		if m.Body != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, m.Body, tc.want)
		}
		if w := m.Webhook.(event.TendWebhook); tc.ev.Type == "heartbeat.missed" &&
			w.Body != "db-backup reported a failure\n\nLast output:\n"+strings.Join(lines[20:], "\n") {
			t.Errorf("%s: the webhook's text changed: %q", tc.name, w.Body)
		}
	}
}

// A recovery links to its heartbeat too, and the detail that names it does
// not reach the webhook's body.
func TestChatTextLinksARecovery(t *testing.T) {
	at := time.Date(2026, 10, 21, 14, 10, 0, 0, time.UTC)
	ev := heartbeat.RecoveredEvent(1, 4, "db-backup")
	ev.ID, ev.CreatedAt = 12, at
	m := messageFor(ev, "primary", ChatOptions{BaseURL: "https://tend.example", OutputLines: 10})
	if m.Subject != "Heartbeat recovered: db-backup" || m.Body != "https://tend.example/heartbeats/4" {
		t.Errorf("subject %q body %q", m.Subject, m.Body)
	}
	if m := messageFor(ev, "primary", chat10); m.Body != "" {
		t.Errorf("without a base URL: body %q, want none", m.Body)
	}
	plain := ev
	plain.Detail = ""
	got, err := json.Marshal(messageFor(ev, "primary", chat10).Webhook)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(messageFor(plain, "primary", chat10).Webhook)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the detail changed the webhook body:\n got %s\nwant %s", got, want)
	}
}

// The tail of a run whose output was cut at 1 MiB says it comes from before
// the cut, not from the end of what the run printed.
func TestChatTextLabelsATruncatedTail(t *testing.T) {
	ev := core.Event{Type: "run.failed",
		Payload: `{"run_id":21,"job_id":3,"job_name":"chatty","status":"failed","exit_code":1}`,
		Detail:  `{"output":"line 1\nline 2","truncated":true}`}
	m := messageFor(ev, "primary", chat10)
	if want := "Last output before the 1 MiB cut:\nline 1\nline 2"; m.Body != want {
		t.Errorf("body %q, want %q", m.Body, want)
	}
}

// A probe's or a drill's alert shows its verdict (its FAIL: lines, or why it
// could not check) before the tail of what followed it, within the same line
// budget: the verdict first, the tail with what is left.
func TestChatTextLeadsWithTheVerdict(t *testing.T) {
	ev := core.Event{Type: "run.failed",
		Payload: `{"run_id":9,"job_id":4,"job_name":"web-cert","status":"failed","exit_code":1}`,
		Detail: `{"verdict":"FAIL: expires in 4 days\nFAIL: renewal missed\nFAIL: chain does not verify",` +
			`"output":"subject: CN=tend.test\nissuer: CN=Test CA\nnot_after: 2026-10-25\nchain: verified"}`}
	for _, tc := range []struct {
		lines int
		want  string
	}{
		{10, "FAIL: expires in 4 days\nFAIL: renewal missed\nFAIL: chain does not verify\n\n" +
			"Last output:\nsubject: CN=tend.test\nissuer: CN=Test CA\nnot_after: 2026-10-25\nchain: verified"},
		{4, "FAIL: expires in 4 days\nFAIL: renewal missed\nFAIL: chain does not verify\n\nLast output:\nchain: verified"},
		{2, "FAIL: expires in 4 days\nFAIL: renewal missed"},
		{0, ""},
	} {
		if m := messageFor(ev, "primary", ChatOptions{OutputLines: tc.lines}); m.Body != tc.want || m.Subject != "Job failed: web-cert (exit 1)" {
			t.Errorf("%d lines: subject %q body\n%q\nwant\n%q", tc.lines, m.Subject, m.Body, tc.want)
		}
	}
}
