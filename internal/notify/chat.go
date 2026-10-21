package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
)

// ChatOptions shape the text of the chat alerts.
type ChatOptions struct {
	// BaseURL is Tend's external URL (config.LoadBaseURL: TEND_BASE_URL),
	// which the alerts link to the run or the heartbeat from; "" leaves the
	// links out.
	BaseURL string
	// OutputLines is how many lines of output (a failed run's, a failure
	// ping's) an alert shows (config.LoadAlertOutputLines:
	// TEND_ALERT_OUTPUT_LINES); 0 leaves the output out. An event keeps at
	// most core.MaxOutputTailLines.
	OutputLines int
}

// chatText is the subject and body a person reads: the alert as Slack,
// Discord, email, Telegram, ntfy and Pushover show it, often on a phone. The
// subject names the job or heartbeat and the body never repeats it; the body
// says what the subject does not: when a heartbeat last pinged and what it
// expects, the run that did not finish, the last lines a failed run or a
// failure ping left (as many lines as chat.OutputLines), the timeout a run
// reached. When chat.BaseURL is set, a link to the run's or the heartbeat's
// page follows what happened and comes before any output, so a channel that
// cuts long messages (Pushover, at 1024 characters) cuts the output, not the
// link. Events that already read well keep render's text. The webhook posts
// render's text, byte for byte, whatever this says.
func chatText(ev core.Event, chat ChatOptions) (subject, body string) {
	subject, body = render(ev)
	var lead, output, page string
	switch ev.Type {
	case "run.failed":
		subject, lead, output = chatRunFailed(ev)
		page = runPage(ev)
	case "run.recovered":
		lead, page = "Its previous run had not succeeded.", runPage(ev)
	case "heartbeat.missed", "heartbeat.recovered":
		if ev.Type == "heartbeat.missed" {
			lead, output = chatMissed(ev)
		}
		if id := heartbeat.IDOf(ev.Detail); id > 0 {
			page = fmt.Sprintf("/heartbeats/%d", id)
		}
	default:
		return subject, body
	}
	var parts []string
	if output = core.TailLines(output, chat.OutputLines); output != "" {
		output = "Last output:\n" + output
	}
	for _, part := range []string{lead, link(chat.BaseURL, page), output} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return subject, strings.Join(parts, "\n\n")
}

// link is the URL of the dashboard page at path under base, or "" without
// either.
func link(base, path string) string {
	if base == "" || path == "" {
		return ""
	}
	return base + path
}

// runPage is the dashboard path of a run event's run, or "" when its payload
// names none.
func runPage(ev core.Event) string {
	var p struct {
		RunID int64 `json:"run_id"`
	}
	if json.Unmarshal([]byte(ev.Payload), &p) != nil || p.RunID <= 0 {
		return ""
	}
	return fmt.Sprintf("/runs/%d", p.RunID)
}

// runFailure is the part of a run.failed's detail an alert uses (the
// runner's jobs.FailureDetail; notify does not import jobs).
type runFailure struct {
	Output         string `json:"output"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// chatRunFailed says which job failed and how: its exit code in the subject,
// or that it timed out in the subject and the timeout in lead, and the tail
// of its output the event kept.
func chatRunFailed(ev core.Event) (subject, lead, output string) {
	var p struct {
		JobName  string `json:"job_name"`
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}
	_ = json.Unmarshal([]byte(ev.Payload), &p)
	var d runFailure
	_ = json.Unmarshal([]byte(ev.Detail), &d)

	what := "Job failed"
	if p.Status == "timed_out" {
		what = "Job timed out"
	}
	subject = what
	if p.JobName != "" {
		subject += ": " + p.JobName
	}
	if p.Status != "timed_out" && p.ExitCode > 0 {
		subject += fmt.Sprintf(" (exit %d)", p.ExitCode)
	}
	if p.Status == "timed_out" && d.TimeoutSeconds > 0 {
		lead = "It was stopped after its " + span(seconds(d.TimeoutSeconds)) + " timeout."
	}
	return subject, lead, d.Output
}

// chatMissed is what a heartbeat.missed says beyond its subject: for a
// heartbeat that stopped pinging, how long it has been quiet and what it
// expects; for a run that did not finish, when it started and how long it was
// allowed; for a failure, the tail of what the ping logged as output (the
// subject has the exit status). A miss recorded before misses carried a
// detail has nothing more to say.
func chatMissed(ev core.Event) (lead, output string) {
	d, ok := heartbeat.ParseMissDetail(ev.Detail)
	if !ok {
		return "", ""
	}
	switch d.Reason {
	case heartbeat.ReasonFailed:
		return "", d.Output
	case heartbeat.ReasonOverran:
		return fmt.Sprintf("A run started at %s and did not finish within %s.",
			chatTime(d.StartedAt), span(seconds(d.GraceSeconds))), ""
	case heartbeat.ReasonMissed:
		expected := ""
		if d.PeriodSeconds > 0 {
			grace := "no grace"
			if d.GraceSeconds > 0 {
				grace = span(seconds(d.GraceSeconds)) + " of grace"
			}
			expected = fmt.Sprintf("; one is expected every %s, with %s", span(seconds(d.PeriodSeconds)), grace)
		}
		switch {
		case d.LastSeenAt.IsZero():
			return "It has never pinged" + expected + ".", ""
		case ev.CreatedAt.After(d.LastSeenAt):
			return fmt.Sprintf("No ping for %s, since %s%s.", span(ev.CreatedAt.Sub(d.LastSeenAt)), chatTime(d.LastSeenAt), expected), ""
		default:
			return "No ping since " + chatTime(d.LastSeenAt) + expected + ".", ""
		}
	}
	return "", ""
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// chatTime is a moment as an alert gives it: to the minute, in UTC.
func chatTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// span is a duration as an alert gives it: its largest unit and the one
// below it, "1d 1h", "5m", "1m 30s", with the rest dropped.
func span(d time.Duration) string {
	units := [...]struct {
		size time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}}
	for i, u := range units {
		if d < u.size {
			continue
		}
		s := fmt.Sprintf("%d%s", d/u.size, u.name)
		if i+1 < len(units) {
			if n := d % u.size / units[i+1].size; n > 0 {
				s += fmt.Sprintf(" %d%s", n, units[i+1].name)
			}
		}
		return s
	}
	return "0s"
}
