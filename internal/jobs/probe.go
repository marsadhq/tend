package jobs

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// day is the unit the probes count in.
const day = 24 * time.Hour

// What the probes' runs record as their exit code: 0 when everything checked
// was fine, 1 when a check failed (or the answer lacked what it needs), and -1
// when the probe could not check at all (no connection, no answer). A probe
// that timed out records 0 with the timed_out status, like a shell job.
const (
	probeExitFailed = 1
	probeExitError  = -1
)

// now is the time the probes judge expiry dates by.
func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// specOf returns j's settings, or the zero Spec when it has none (which the
// type's validation then refuses).
func specOf(j Job) Spec {
	if j.Spec == nil {
		return Spec{}
	}
	return *j.Spec
}

// probeReport is what a probe found: problems fail the run, info lines say
// what was checked.
type probeReport struct {
	problems []string
	info     []string
}

// fail records a problem.
func (r *probeReport) fail(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

// add records an info line "key: value", the values aligned.
func (r *probeReport) add(key, format string, args ...any) {
	r.info = append(r.info, fmt.Sprintf("%-12s %s", key+":", fmt.Sprintf(format, args...)))
}

// result turns r into a run's result. The output starts with the verdict, so
// it is what a glance at the run shows: a "FAIL: " line per problem (the run
// failed, exit code 1) or one "ok: " line with okLine (succeeded), then a
// blank line and the info lines.
func (r probeReport) result(attempt int, started time.Time, okLine string) RunResult {
	var b strings.Builder
	if len(r.problems) > 0 {
		for _, p := range r.problems {
			b.WriteString("FAIL: " + p + "\n")
		}
	} else {
		b.WriteString("ok: " + okLine + "\n")
	}
	if len(r.info) > 0 {
		b.WriteString("\n" + strings.Join(r.info, "\n"))
	}
	res := RunResult{
		Status:  StatusSucceeded,
		Output:  strings.TrimRight(b.String(), "\n"),
		Attempt: attempt,
		Started: started,
		Ended:   time.Now(),
	}
	if len(r.problems) > 0 {
		res.Status, res.ExitCode = StatusFailed, probeExitFailed
	}
	return res
}

// probeErrorResult is the result of a probe that could not check at all: a
// failed run with exit code -1 and the error, or a timed out one when ctx's
// deadline is what stopped it.
func probeErrorResult(ctx context.Context, attempt int, started time.Time, err error) RunResult {
	res := RunResult{
		Status:   StatusFailed,
		ExitCode: probeExitError,
		Output:   "error: " + err.Error(),
		Attempt:  attempt,
		Started:  started,
		Ended:    time.Now(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.Status, res.ExitCode = StatusTimedOut, 0
		res.Output = "timed out: " + err.Error()
	}
	return res
}

// humanDays renders a span as whole days, as whole hours under two days, and
// as whole minutes under an hour. A negative span is rendered as its length.
func humanDays(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d >= 2*day:
		return fmt.Sprintf("%d days", int64(d/day))
	case d >= time.Hour:
		return plural(int64(d/time.Hour), "hour")
	}
	return plural(int64(d/time.Minute), "minute")
}

// plural renders "1 hour", "3 hours".
func plural(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// floorDays is the number of whole days in d, rounded towards minus infinity,
// so a date a few hours past is -1 days away, not 0.
func floorDays(d time.Duration) int64 {
	n := int64(d / day)
	if d < 0 && d%day != 0 {
		n--
	}
	return n
}

// warnText says when an expiry warns: "warns 14 days before".
func warnText(warnDays int) string {
	if warnDays == 0 {
		return "warns once expired"
	}
	return fmt.Sprintf("warns %d days before", warnDays)
}

// dateText renders a time the probes report: RFC 3339 in UTC.
func dateText(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// durationText renders a span without trailing zero units: "48h", "14h24m",
// "1m30s", "1.5s".
func durationText(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Millisecond).String()
	}
	out := d.Round(time.Second).String()
	if strings.HasSuffix(out, "m0s") {
		out = strings.TrimSuffix(out, "0s")
	}
	if strings.HasSuffix(out, "h0m") {
		out = strings.TrimSuffix(out, "0m")
	}
	return out
}
