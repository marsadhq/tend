package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/heartbeat"
)

// chatText is the subject and body a person reads: the alert as Slack,
// Discord, email, Telegram, ntfy and Pushover show it, often on a phone. The
// subject names the job or heartbeat and the body never repeats it; the body
// says what the subject does not: when a heartbeat last pinged and what it
// expects, the run that did not finish, the last lines a failed run or a
// failure ping left, the timeout a run reached. Events that already read
// well keep render's text. The webhook posts render's text, byte for byte,
// whatever this says.
func chatText(ev core.Event) (subject, body string) {
	subject, body = render(ev)
	switch ev.Type {
	case "run.failed":
		return chatRunFailed(ev)
	case "run.recovered":
		return subject, "Its previous run had not succeeded."
	case "heartbeat.missed":
		return subject, chatMissed(ev)
	case "heartbeat.recovered":
		return subject, ""
	}
	return subject, body
}

// runFailure is the part of a run.failed's detail an alert uses (the
// runner's jobs.FailureDetail; notify does not import jobs).
type runFailure struct {
	Output         string `json:"output"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// chatRunFailed says which job failed and how: its exit code in the subject,
// or that it timed out, and the last lines of its output.
func chatRunFailed(ev core.Event) (string, string) {
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
	subject := what
	if p.JobName != "" {
		subject += ": " + p.JobName
	}
	if p.Status != "timed_out" && p.ExitCode > 0 {
		subject += fmt.Sprintf(" (exit %d)", p.ExitCode)
	}
	var parts []string
	if p.Status == "timed_out" && d.TimeoutSeconds > 0 {
		parts = append(parts, "It was stopped after its "+span(seconds(d.TimeoutSeconds))+" timeout.")
	}
	if d.Output != "" {
		parts = append(parts, "Last output:\n"+d.Output)
	}
	return subject, strings.Join(parts, "\n\n")
}

// chatMissed is the body of a heartbeat.missed: for a heartbeat that stopped
// pinging, how long it has been quiet and what it expects; for a run that did
// not finish, when it started and how long it was allowed; for a failure,
// the last lines the ping logged (the subject has the exit status). A miss
// recorded before misses carried a detail has nothing more to say.
func chatMissed(ev core.Event) string {
	d, ok := heartbeat.ParseMissDetail(ev.Detail)
	if !ok {
		return ""
	}
	switch d.Reason {
	case heartbeat.ReasonFailed:
		if d.Output == "" {
			return ""
		}
		return "Last output:\n" + d.Output
	case heartbeat.ReasonOverran:
		return fmt.Sprintf("A run started at %s and did not finish within %s.",
			chatTime(d.StartedAt), span(seconds(d.GraceSeconds)))
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
			return "It has never pinged" + expected + "."
		case ev.CreatedAt.After(d.LastSeenAt):
			return fmt.Sprintf("No ping for %s, since %s%s.", span(ev.CreatedAt.Sub(d.LastSeenAt)), chatTime(d.LastSeenAt), expected)
		default:
			return "No ping since " + chatTime(d.LastSeenAt) + expected + "."
		}
	}
	return ""
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
