package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultExpiryWarnDays is an expiry probe's warn_days: two weeks.
const DefaultExpiryWarnDays = 14

// maxExpiryStdout bounds what is read of an expiry_command's stdout: it has to
// print one date, and anything longer is refused rather than parsed.
const maxExpiryStdout = 4096

// maxExpiryStderr bounds the stderr of an expiry_command kept in the output.
const maxExpiryStderr = 16 << 10

// ParseExpiryDate parses a date an expiry job watches: RFC 3339 with its zone
// (fractional seconds allowed), or a bare date, read as midnight UTC. The
// result is in UTC.
func ParseExpiryDate(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if t, err = time.Parse(time.DateOnly, s); err != nil {
			return time.Time{}, fmt.Errorf("%q is not an RFC 3339 date (2027-01-31T00:00:00Z) or a date (2027-01-31)", s)
		}
	}
	if t.IsZero() {
		return time.Time{}, fmt.Errorf("%q is the zero date", s)
	}
	return t.UTC(), nil
}

// runOnceExpiry runs the expiry branch of runOnce: take the date from the
// job's settings, or from what its expiry_command prints, and judge it.
//
// The command runs exactly as a shell job's command does (shellCommand: sh -c
// as written, the job's env, its own process group, the job's timeout). What
// it prints on stdout is only ever parsed as a date: it is never run, never
// spliced into anything, and refused when it is not a single date. Its stderr
// is kept in the output.
func (e *Executor) runOnceExpiry(ctx context.Context, j Job, env map[string]string, secrets []string, attempt int, started time.Time) RunResult {
	s := specOf(j)
	if err := ValidateSpec(Expiry, &s); err != nil {
		return probeErrorResult(ctx, attempt, started, err)
	}
	if strings.TrimSpace(s.ExpiryCommand) == "" {
		return e.judgeExpiry(s.ExpiresAt.UTC(), "as configured", s.WarnDays, attempt, started)
	}

	stdout := &cappedBuffer{max: maxExpiryStdout}
	// stderr keeps a secret's length more than it shows, so that a secret
	// across the cut is redacted whole first (cutRedacted).
	stderr := &cappedBuffer{max: maxExpiryStderr + secretMargin(secrets)}
	cmd := shellCommand(ctx, s.ExpiryCommand, env)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	status, code := shellOutcome(ctx, cmd.Run())

	var res RunResult
	switch status {
	case StatusTimedOut:
		res = probeErrorResult(ctx, attempt, started, errors.New("expiry_command did not finish"))
	case StatusFailed:
		res = probeErrorResult(ctx, attempt, started, fmt.Errorf("expiry_command exited with status %d", code))
		res.ExitCode = code
	default:
		if t, err := expiryFromStdout(stdout); err != nil {
			res = probeErrorResult(ctx, attempt, started, err)
		} else {
			res = e.judgeExpiry(t, "printed by expiry_command", s.WarnDays, attempt, started)
		}
	}
	errText := cutRedacted(stderr.buf.Bytes(), stderr.truncated, maxExpiryStderr, secrets, "\n... [stderr truncated]")
	if errText = strings.TrimSpace(errText); errText != "" {
		res.Output += "\n\nstderr:\n" + errText
	}
	return res
}

// expiryFromStdout reads the date an expiry_command printed: one line, of one
// date, surrounding white space ignored.
func expiryFromStdout(stdout *cappedBuffer) (time.Time, error) {
	if stdout.truncated {
		return time.Time{}, fmt.Errorf("expiry_command printed more than %d bytes; it must print one date", maxExpiryStdout)
	}
	out := strings.TrimSpace(stdout.String())
	switch {
	case out == "":
		return time.Time{}, errors.New("expiry_command printed nothing; it must print one date")
	case strings.ContainsAny(out, "\r\n"):
		return time.Time{}, errors.New("expiry_command printed more than one line; it must print one date")
	}
	t, err := ParseExpiryDate(out)
	if err != nil {
		// The line is shown whole (at most maxExpiryStdout bytes) and as it
		// was printed, not quoted: cut or escaped, a secret in it would no
		// longer match when the runner redacts the output.
		return time.Time{}, fmt.Errorf("expiry_command printed \"%s\", which is not an RFC 3339 date (2027-01-31T00:00:00Z) or a date (2027-01-31)", out)
	}
	return t, nil
}

// judgeExpiry fails when expires is within warnDays of now, and returns the
// run's result.
func (e *Executor) judgeExpiry(expires time.Time, source string, warnDays, attempt int, started time.Time) RunResult {
	now := e.now()
	left := expires.Sub(now)
	var r probeReport
	r.add("expires_at", "%s (%s)", dateText(expires), source)
	r.add("days_left", "%d", floorDays(left))
	switch {
	case left <= 0:
		r.fail("expired %s ago (%s)", humanDays(left), dateText(expires))
	case left <= time.Duration(warnDays)*day:
		r.fail("expires in %s (%s), within warn_days %d", humanDays(left), dateText(expires), warnDays)
	}
	return r.result(attempt, started, fmt.Sprintf("expires in %s (%s; %s)", humanDays(left), dateText(expires), warnText(warnDays)))
}
