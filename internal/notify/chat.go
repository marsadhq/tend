package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	kitnotify "github.com/marsadhq/kit/notify"

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

// chatText is the subject, body and output a person reads: the alert as
// Slack, Discord, email, Telegram, ntfy and Pushover show it, often on a
// phone (forChannel puts it together for each). The subject names the job or
// heartbeat and the body never repeats it; the body says what the subject
// does not: when a heartbeat last pinged and what it expects, the run that
// did not finish, the timeout a run reached, and, when chat.BaseURL is set, a
// link to the run's or the heartbeat's page. The output is what a failed run
// or a failure ping printed (a probe's or drill's verdict first, then as many
// lines of the tail as chat.OutputLines allows): text Tend did not write,
// kept apart so that Slack and Discord show it as code, and after the body
// elsewhere, so a channel that cuts long messages cuts the output, not the
// link. Events that already read well keep render's text. The webhook posts
// render's text, byte for byte, whatever this says.
func chatText(ev core.Event, chat ChatOptions) (subject, body, output string) {
	subject, body = render(ev)
	var lead, page string
	var out alertOutput
	switch ev.Type {
	case "run.failed":
		subject, lead, out = chatRunFailed(ev)
		page = runPage(ev)
	case "run.recovered":
		lead, page = "Its previous run had not succeeded.", runPage(ev)
	case "heartbeat.missed", "heartbeat.recovered":
		if ev.Type == "heartbeat.missed" {
			lead, out = chatMissed(ev)
		}
		if id := heartbeat.IDOf(ev.Detail); id > 0 {
			page = fmt.Sprintf("/heartbeats/%d", id)
		}
	default:
		return subject, body, ""
	}
	return subject, joinParts(lead, link(chat.BaseURL, page)), out.text(chat.OutputLines)
}

// joinParts joins the parts that are not empty with a blank line.
func joinParts(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

// pushoverMaxMessage is the most characters of a message Pushover keeps; kit
// cuts the rest off its end.
const pushoverMaxMessage = 1024

// forChannel is m as a channel of kind gets it. Slack and Discord show
// m.Output themselves, apart from the text Tend wrote: in a code block, where
// it can neither mention anyone nor make a link with a label of its own (kit).
// The other channels show plain text, so the output follows the body after a
// blank line; for Pushover it is cut from its start to fit what Pushover
// keeps, so that the link before it and the end of the output, where the
// error usually is, both arrive.
func forChannel(kind kitnotify.ChannelType, m kitnotify.Message) kitnotify.Message {
	switch kind {
	case kitnotify.Slack, kitnotify.Discord, kitnotify.Webhook:
		return m
	case kitnotify.Pushover:
		m.Output = fitEnd(m.Output, pushoverMaxMessage-utf8.RuneCountInString(m.Body)-2)
	}
	m.Body, m.Output = joinParts(m.Body, m.Output), ""
	return m
}

// fitEnd is s within room characters: as it is when it fits, otherwise its
// end after "…", or nothing when there is no room for that.
func fitEnd(s string, room int) string {
	if utf8.RuneCountInString(s) <= room {
		return s
	}
	if room < 2 {
		return ""
	}
	r := []rune(s)
	return "…" + string(r[len(r)-(room-1):])
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

// alertOutput is what an alert shows of a failure's output: a probe's or a
// drill's verdict, from its start, and the tail the event kept, from its
// end, under a label.
type alertOutput struct {
	verdict string
	tail    string
	// truncated says the tail comes from before a 1 MiB cut.
	truncated bool
}

// text is the output section of an alert showing at most lines lines, the
// verdict's first and the tail's with what is left, or "" when there is
// nothing to show.
func (o alertOutput) text(lines int) string {
	var parts []string
	verdict := core.HeadLines(o.verdict, lines)
	if verdict != "" {
		parts = append(parts, verdict)
		lines -= strings.Count(verdict, "\n") + 1
	}
	if tail := core.TailLines(o.tail, lines); tail != "" {
		label := "Last output:"
		if o.truncated {
			label = "Last output before the 1 MiB cut:"
		}
		parts = append(parts, label+"\n"+tail)
	}
	return strings.Join(parts, "\n\n")
}

// runFailure is the part of a run.failed's detail an alert uses (the
// runner's jobs.FailureDetail; notify does not import jobs).
type runFailure struct {
	Verdict        string `json:"verdict"`
	Output         string `json:"output"`
	Truncated      bool   `json:"truncated"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	HTTPStatus     int    `json:"http_status"`
}

// chatRunFailed says which job failed and how: its exit code in the subject,
// or that it timed out in the subject and the timeout in lead, and the tail
// of its output the event kept.
func chatRunFailed(ev core.Event) (subject, lead string, output alertOutput) {
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
	switch {
	case p.Status == "timed_out":
	case d.HTTPStatus > 0:
		subject += fmt.Sprintf(" (HTTP %d)", d.HTTPStatus)
	case p.ExitCode > 0:
		subject += fmt.Sprintf(" (exit %d)", p.ExitCode)
	}
	if p.Status == "timed_out" && d.TimeoutSeconds > 0 {
		lead = "It was stopped after its " + span(seconds(d.TimeoutSeconds)) + " timeout."
	}
	return subject, lead, alertOutput{verdict: d.Verdict, tail: d.Output, truncated: d.Truncated}
}

// chatMissed is what a heartbeat.missed says beyond its subject: for a
// heartbeat that stopped pinging, how long it has been quiet and what it
// expects; for a run that did not finish, when it started and how long it was
// allowed; for a failure, the tail of what the ping logged as output (the
// subject has the exit status). A miss recorded before misses carried a
// detail has nothing more to say.
func chatMissed(ev core.Event) (lead string, output alertOutput) {
	d, ok := heartbeat.ParseMissDetail(ev.Detail)
	if !ok {
		return "", alertOutput{}
	}
	switch d.Reason {
	case heartbeat.ReasonFailed:
		return "", alertOutput{tail: d.Output}
	case heartbeat.ReasonOverran:
		return fmt.Sprintf("A run started at %s and did not finish within %s.",
			chatTime(d.StartedAt), span(seconds(d.GraceSeconds))), alertOutput{}
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
			return "It has never pinged" + expected + ".", alertOutput{}
		case ev.CreatedAt.After(d.LastSeenAt):
			return fmt.Sprintf("No ping for %s, since %s%s.", span(ev.CreatedAt.Sub(d.LastSeenAt)), chatTime(d.LastSeenAt), expected), alertOutput{}
		default:
			return "No ping since " + chatTime(d.LastSeenAt) + expected + ".", alertOutput{}
		}
	}
	return "", alertOutput{}
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
