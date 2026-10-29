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
// did not finish, the timeout a run reached, for a reminder that a job is
// still failing how many runs in a row have failed since when, for a
// recovery the streak it ended, and, when chat.BaseURL is set, a link to the
// run's or the heartbeat's page. The output is what a failed run
// or a failure ping printed (a probe's or drill's verdict first, then as many
// lines of the tail as chat.OutputLines allows): text Tend did not write,
// kept apart so that Slack and Discord show it as code, and after the body
// elsewhere, so a channel that cuts long messages cuts the output, not the
// link. Events that already read well keep render's text. The webhook posts
// render's text, byte for byte, whatever this says.
func chatText(ev core.Event, chat ChatOptions) (subject, body string, out alertOutput) {
	subject, body = render(ev)
	var lead, page string
	switch ev.Type {
	case "run.failed":
		subject, lead, out = chatRunFailed(ev)
		page = runPage(ev)
	case "run.recovered":
		lead, page = chatRecovered(ev), runPage(ev)
	case "heartbeat.missed", "heartbeat.recovered":
		if ev.Type == "heartbeat.missed" {
			lead, out = chatMissed(ev)
		}
		if id := heartbeat.IDOf(ev.Detail); id > 0 {
			page = fmt.Sprintf("/heartbeats/%d", id)
		}
	default:
		return subject, body, alertOutput{}
	}
	return subject, joinParts(lead, link(chat.BaseURL, page)), out
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

// channelMessage is the message for ev as a channel of kind gets it. Slack
// and Discord show m.Output themselves, apart from the text Tend wrote: in a
// code block, where it can neither mention anyone nor make a link with a
// label of its own (kit). The other channels show plain text, so the output
// follows the body after a blank line. Where a channel keeps only so much
// (Discord's 2000 units, Pushover's 1024 characters) the output is fitted to
// the room the rest leaves (alertOutput.fit): a probe's or drill's verdict
// whole, and the tail cut from its start, so the link, the reason the run
// failed and the end of its output all arrive.
func channelMessage(kind kitnotify.ChannelType, ev core.Event, instance string, chat ChatOptions) kitnotify.Message {
	m, out := compose(ev, instance, chat)
	lines := chat.OutputLines
	switch kind {
	case kitnotify.Slack, kitnotify.Webhook:
		m.Output = out.text(lines)
	case kitnotify.Discord:
		m.Output = out.fit(lines, kitnotify.DiscordOutputRoom(m), kitnotify.DiscordOutputLength)
	case kitnotify.Pushover:
		room := pushoverMaxMessage - utf8.RuneCountInString(m.Body)
		if m.Body != "" {
			room -= 2 // the blank line between them
		}
		m.Body = joinParts(m.Body, out.fit(lines, room, utf8.RuneCountInString))
	default:
		m.Body = joinParts(m.Body, out.text(lines))
	}
	return m
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

// fit is text held to budget, as length measures it: when the whole does not
// fit, the verdict is kept and the tail is cut from its start (after "…") to
// fill what is left; a verdict that alone does not fit is cut at its end.
func (o alertOutput) fit(lines, budget int, length func(string) int) string {
	full := o.text(lines)
	if length(full) <= budget {
		return full
	}
	verdict := core.HeadLines(o.verdict, lines)
	if verdict != "" && length(verdict) >= budget {
		return cutEnd(verdict, budget, length)
	}
	label := "Last output:"
	if o.truncated {
		label = "Last output before the 1 MiB cut:"
	}
	head := label + "\n…"
	if verdict != "" {
		head = verdict + "\n\n" + head
		lines -= strings.Count(verdict, "\n") + 1
	}
	room := budget - length(head)
	tail := core.TailLines(o.tail, lines)
	if room <= 0 || tail == "" {
		return verdict
	}
	return head + cutStart(tail, room, length)
}

// cutStart is the longest end of s, whole characters, that length counts at
// most room.
func cutStart(s string, room int, length func(string) int) string {
	used, start := 0, len(s)
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:start])
		if used += length(string(r)); used > room {
			break
		}
		start -= size
	}
	return s[start:]
}

// cutEnd is the longest start of s, whole characters, that fits budget
// together with the "…" it ends with.
func cutEnd(s string, budget int, length func(string) int) string {
	room := budget - length("…")
	used := 0
	for i, r := range s {
		if used += length(string(r)); used > room {
			return s[:i] + "…"
		}
	}
	return s
}

// runFailure is the part of a run.failed's detail an alert uses (the
// runner's jobs.FailureDetail; notify does not import jobs).
type runFailure struct {
	Verdict        string `json:"verdict"`
	Output         string `json:"output"`
	Truncated      bool   `json:"truncated"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	HTTPStatus     int    `json:"http_status"`
	runStreak
}

// runStreak is the failing streak a run event's detail records (the
// store's jobs.Streak): with a run.failed, the runs in a row that have
// failed, this one included; with a run.recovered, the streak it ended.
type runStreak struct {
	FailedRuns   int       `json:"failed_runs"`
	FailingSince time.Time `json:"failing_since"`
}

// streakSpan is how long the streak had lasted at t, as " (3h)", or "" when
// that is not known.
func (s runStreak) streakSpan(t time.Time) string {
	if s.FailingSince.IsZero() || !t.After(s.FailingSince) {
		return ""
	}
	return " (" + span(t.Sub(s.FailingSince)) + ")"
}

// chatRunFailed says which job failed and how: its exit code in the subject,
// or that it timed out in the subject and the timeout in lead, and the tail
// of its output the event kept. A failure after the first of a streak (a
// reminder) says the job is still failing, and since when, how many runs.
func chatRunFailed(ev core.Event) (subject, lead string, output alertOutput) {
	var p struct {
		JobName  string `json:"job_name"`
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}
	_ = json.Unmarshal([]byte(ev.Payload), &p)
	var d runFailure
	_ = json.Unmarshal([]byte(ev.Detail), &d)

	reminder := d.FailedRuns > 1
	what := "Job failed"
	switch {
	case reminder:
		what = "Job still failing"
	case p.Status == "timed_out":
		what = "Job timed out"
	}
	subject = what
	if p.JobName != "" {
		subject += ": " + p.JobName
	}
	switch {
	case p.Status == "timed_out":
		if reminder {
			subject += " (timed out)"
		}
	case d.HTTPStatus > 0:
		subject += fmt.Sprintf(" (HTTP %d)", d.HTTPStatus)
	case p.ExitCode > 0:
		subject += fmt.Sprintf(" (exit %d)", p.ExitCode)
	}
	var says []string
	if reminder {
		line := fmt.Sprintf("%d runs in a row have failed", d.FailedRuns)
		if !d.FailingSince.IsZero() {
			line += ", since " + chatTime(d.FailingSince) + d.streakSpan(ev.CreatedAt)
		}
		says = append(says, line+".")
	}
	if p.Status == "timed_out" && d.TimeoutSeconds > 0 {
		says = append(says, "It was stopped after its "+span(seconds(d.TimeoutSeconds))+" timeout.")
	}
	return subject, strings.Join(says, " "), alertOutput{verdict: d.Verdict, tail: d.Output, truncated: d.Truncated}
}

// chatRecovered is what a run.recovered says beyond its subject: the streak
// it ended, when there was one of more than one run, or that the previous
// run had not succeeded.
func chatRecovered(ev core.Event) string {
	var s runStreak
	_ = json.Unmarshal([]byte(ev.Detail), &s)
	if s.FailedRuns <= 1 {
		return "Its previous run had not succeeded."
	}
	line := fmt.Sprintf("It succeeded after %d failed runs in a row", s.FailedRuns)
	if !s.FailingSince.IsZero() {
		line += ", since " + chatTime(s.FailingSince) + s.streakSpan(ev.CreatedAt)
	}
	return line + "."
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
