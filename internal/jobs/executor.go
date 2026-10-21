// Package jobs - executor.go runs a single Job (shell, HTTP, or one of the
// probes and drills) with timeout, retry-with-backoff, and captures its output
// + exit code.
package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marsadhq/kit/urlredact"
)

// killGraceDelay bounds how long cmd.Wait will block after the per-attempt
// context is cancelled: it gives I/O pipes a brief window to drain, then
// force-closes them so Run can never hang waiting on a stuck/forked child.
const killGraceDelay = 5 * time.Second

// DefaultTimeout is the per-attempt execution timeout applied when a job's
// TimeoutSeconds is unset. Shared with the runner's stale-run reaper so both
// agree on when an unfinished run is overdue.
const DefaultTimeout = 30 * time.Minute

// maxOutputBytes caps the captured output of one attempt (combined
// stdout+stderr for shell jobs, response body for HTTP jobs) so a chatty or
// runaway job cannot exhaust memory or bloat the job_runs table. Excess output
// is discarded, never fails the run.
const maxOutputBytes = 1 << 20 // 1 MiB

// truncationMarker is appended to captured output that hit maxOutputBytes.
const truncationMarker = "\n... [output truncated at 1MiB]"

// cappedBuffer is an io.Writer keeping at most max bytes and discarding the
// rest, recording that truncation happened. Write never errors, so a child
// process is never killed by a full pipe — its extra output just vanishes.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

// String returns the kept output, with the truncation marker when the cap hit.
func (c *cappedBuffer) String() string {
	if c.truncated {
		return string(trimPartialRune(c.buf.Bytes())) + truncationMarker
	}
	return c.buf.String()
}

// trimPartialRune drops a trailing, incomplete UTF-8 sequence from b. The cap
// cuts at a byte offset, which can fall inside a multi-byte character; the
// leftover bytes would make otherwise valid output invalid UTF-8, which a
// Postgres TEXT column refuses to store. Only a sequence the cut left
// unfinished is removed: complete characters, and bytes that were never valid
// UTF-8 in the first place, are kept as they are.
func trimPartialRune(b []byte) []byte {
	// A sequence is at most utf8.UTFMax bytes long, so an unfinished one starts
	// within the last UTFMax-1 bytes.
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		if start := len(b) - i; utf8.RuneStart(b[start]) {
			if !utf8.FullRune(b[start:]) {
				return b[:start]
			}
			return b
		}
	}
	return b
}

// RunResult holds the outcome of one complete Run (across all attempts).
type RunResult struct {
	Status   RunStatus
	ExitCode int
	Output   string
	Attempt  int
	Started  time.Time
	Ended    time.Time
}

// Executor runs jobs. Zero value is usable via NewExecutor.
type Executor struct {
	// Backoff, if non-nil, returns the wait duration before attempt n+1.
	// Tests set this to func(int) time.Duration { return 0 } for instant retries.
	// If nil, the default exponential backoff is used.
	Backoff func(attempt int) time.Duration

	// Now, if non-nil, is the time the tls, domain and expiry probes judge
	// expiry dates by; nil means time.Now. Tests set it.
	Now func() time.Time

	// RDAPBootstrapURL is where domain probes read the RDAP bootstrap file
	// that names each TLD's RDAP server; empty means IANA's
	// (IANABootstrapURL). Tests point it at a local server.
	RDAPBootstrapURL string

	// RDAPTransport, if non-nil, carries the domain probes' requests in place
	// of Go's default transport. Tests set it to trust their TLS servers.
	RDAPTransport http.RoundTripper
}

// NewExecutor returns a ready-to-use Executor with default backoff.
func NewExecutor() *Executor {
	return &Executor{}
}

// backoff returns the wait time before re-attempting after attempt n.
// Defaults to attempt² seconds (1s, 4s, 9s, …).
func (e *Executor) backoff(attempt int) time.Duration {
	if e.Backoff != nil {
		return e.Backoff(attempt)
	}
	return time.Duration(attempt*attempt) * time.Second
}

// forever is the longest Duration there is. Time-limit arithmetic saturates at
// it instead of overflowing.
const forever = time.Duration(math.MaxInt64)

// seconds converts a number of seconds to a Duration, saturating at forever
// (or its negative) instead of wrapping around. A Duration counts nanoseconds,
// so a timeout_seconds past roughly 292 years overflows a plain multiplication
// and comes out as an arbitrary value, negative as often as not.
func seconds(n int) time.Duration {
	const limit = int64(forever / time.Second)
	switch {
	case int64(n) > limit:
		return forever
	case int64(n) < -limit:
		return -forever
	}
	return time.Duration(n) * time.Second
}

// maxRunDuration returns the longest one complete Run of j can legitimately
// take: every attempt running into its timeout, the grace period each kill is
// given, and every backoff pause in between. The runner's stale-run reaper
// uses it, so the reaper and the retry loop in Run agree on when a run is
// overdue. The result saturates rather than overflowing, for an enormous
// timeout as much as for an absurd retry count: a limit that wrapped around to
// a negative value would have the reaper fail the run as soon as reapSlack had
// passed.
func (e *Executor) maxRunDuration(j Job) time.Duration {
	attempts := j.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	timeout := seconds(j.TimeoutSeconds)
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	grace := killGraceDelay
	if j.Type == Drill {
		// A drill's cleanup runs after its attempt's timeout, within a bound
		// of its own.
		grace += drillCleanupTimeout + killGraceDelay
	}
	if timeout > forever-grace {
		return forever
	}
	perAttempt := timeout + grace

	total := perAttempt
	for attempt := 1; attempt < attempts; attempt++ {
		pause := e.backoff(attempt)
		if pause < 0 {
			pause = 0
		}
		if total > forever-pause-perAttempt {
			return forever
		}
		total += pause + perAttempt
	}
	return total
}

// Run executes j, retrying up to j.MaxRetries additional times on any
// non-succeeded outcome - including StatusTimedOut. Each failed attempt
// is followed by a backoff pause before the next attempt.
// Worst-case wall time ≈ (MaxRetries+1)*TimeoutSeconds + Σ(i² for i in 1..MaxRetries)
// seconds when using the default exponential backoff; see maxRunDuration.
func (e *Executor) Run(ctx context.Context, j Job, env map[string]string) RunResult {
	return e.run(ctx, j, env, nil)
}

// run is Run with the plaintext values of the secrets in env. The runner
// passes them so that a shell job's, an expiry command's and a drill's output,
// which are cut to a size before they are stored, are redacted before the cut:
// a secret that straddled a cut would otherwise be kept in part, which no
// longer matches the secret when the runner redacts the stored output.
func (e *Executor) run(ctx context.Context, j Job, env map[string]string, secrets []string) RunResult {
	maxAttempts := j.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var last RunResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		last = e.runOnce(ctx, j, env, secrets, attempt)
		if last.Status == StatusSucceeded {
			return last
		}

		// Stop retrying if we've exhausted attempts.
		if attempt >= maxAttempts {
			break
		}

		// Stop retrying if the parent context is already cancelled/timed-out.
		if ctx.Err() != nil {
			return last
		}

		// Cancellable backoff between attempts.
		d := e.backoff(attempt)
		if d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return last // parent cancelled: stop retrying
			case <-t.C:
			}
		}
	}
	return last
}

// runOnce executes j exactly once with a per-attempt timeout. The timeout
// saturates like the limit in maxRunDuration, so an enormous timeout_seconds
// means "no timeout to speak of" to both, not an attempt that is cut short the
// moment it starts. Like maxRunDuration it treats any timeout that is not
// positive as unset: a negative one (which job input now refuses, but a row
// written before that may still hold) used to time every attempt out at once
// while the reaper allowed the run the default.
func (e *Executor) runOnce(ctx context.Context, j Job, env map[string]string, secrets []string, attempt int) RunResult {
	timeout := seconds(j.TimeoutSeconds)
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()

	switch j.Type {
	case HTTP:
		return e.runOnceHTTP(cctx, j, attempt, started)
	case TLS:
		return e.runOnceTLS(cctx, j, attempt, started)
	case Domain:
		return e.runOnceDomain(cctx, j, attempt, started)
	case Expiry:
		return e.runOnceExpiry(cctx, j, env, secrets, attempt, started)
	case Drill:
		return e.runOnceDrill(cctx, j, env, secrets, attempt, started)
	default: // Shell or empty/default
		return e.runOnceShell(cctx, j, env, secrets, attempt, started)
	}
}

// runOnceShell runs the shell branch of runOnce. The capture keeps the
// longest secret's length past maxOutputBytes, so that a secret the cap
// falls inside is captured whole (shellOutput).
func (e *Executor) runOnceShell(cctx context.Context, j Job, env map[string]string, secrets []string, attempt int, started time.Time) RunResult {
	cmd := shellCommand(cctx, j.Command, env)

	buf := &cappedBuffer{max: maxOutputBytes + secretMargin(secrets)}
	cmd.Stdout = buf
	cmd.Stderr = buf

	err := cmd.Run()

	res := RunResult{
		Output:  shellOutput(buf, secrets),
		Attempt: attempt,
		Started: started,
		Ended:   time.Now(),
	}
	res.Status, res.ExitCode = shellOutcome(cctx, err)
	return res
}

// shellOutput is what a shell job's capture c hands the runner, which redacts
// the job's secrets and then cuts the text to maxOutputBytes (storedOutput).
// When the job printed no more than c keeps, that is all of it: every secret
// in it is whole. When it printed more, it is the part headOf keeps of the
// first maxOutputBytes, followed by the truncation marker.
func shellOutput(c *cappedBuffer, secrets []string) string {
	if !c.truncated {
		return c.buf.String()
	}
	head, _ := headOf(c.buf.Bytes(), maxOutputBytes, secrets, true)
	return string(head) + truncationMarker
}

// shellCommand builds the command every shell-backed step runs: a shell job's
// command, an expiry job's expiry_command and a drill's restore, checks and
// cleanup. command is run by `sh -c` exactly as written; Tend never splices a
// value into it (what a step needs to know, such as a drill's scratch
// directory, reaches it through env).
func shellCommand(ctx context.Context, command string, env map[string]string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	// Make the shell the leader of its own process group and SIGKILL the whole
	// group on cancellation, so children the job forks are killed too.
	configureProcessGroup(cmd)
	// Backstop: bound Wait even if I/O pipes linger after the group is killed.
	cmd.WaitDelay = killGraceDelay
	// Overlay job env vars on top of the inherited OS environment so that
	// commands like "sleep" (found via PATH) continue to work.
	cmd.Env = append(os.Environ(), toEnvSlice(env)...)
	return cmd
}

// shellOutcome maps what cmd.Run returned for a command run under ctx to a run
// status and exit code: timed out when ctx's deadline passed, succeeded on a
// zero exit, otherwise failed with the exit code (-1 when the command did not
// get as far as exiting, e.g. sh could not be started).
func shellOutcome(ctx context.Context, err error) (RunStatus, int) {
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return StatusTimedOut, 0
	case err == nil:
		return StatusSucceeded, 0
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return StatusFailed, ee.ExitCode()
		}
		return StatusFailed, -1
	}
}

// runOnceHTTP runs the HTTP branch of runOnce.
func (e *Executor) runOnceHTTP(cctx context.Context, j Job, attempt int, started time.Time) RunResult {
	method := j.HTTPMethod
	if method == "" {
		method = http.MethodGet
	}

	var bodyReader io.Reader
	if j.HTTPBody != "" {
		bodyReader = strings.NewReader(j.HTTPBody)
	}

	// HTTPURL may embed a secret token in its path or query string, and the
	// errors below are stored as job_runs.output, which is retained and shown
	// by the CLI, the API and the dashboard. Both error branches therefore
	// report only the host and the underlying cause, never the URL.
	req, err := http.NewRequestWithContext(cctx, method, j.HTTPURL, bodyReader)
	if err != nil {
		return RunResult{
			Status:   StatusFailed,
			ExitCode: -1,
			Output:   urlredact.Error(j.HTTPURL, err).Error(),
			Attempt:  attempt,
			Started:  started,
			Ended:    time.Now(),
		}
	}

	resp, err := http.DefaultClient.Do(req)
	res := RunResult{
		Attempt: attempt,
		Started: started,
	}

	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			res.Status = StatusTimedOut
		} else {
			res.Status = StatusFailed
			res.ExitCode = -1
			res.Output = urlredact.Error(j.HTTPURL, err).Error()
		}
		res.Ended = time.Now()
		return res
	}
	defer resp.Body.Close()

	// Read at most one byte past the cap: the extra byte detects truncation
	// without ever buffering an unbounded response.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxOutputBytes+1))
	if readErr != nil {
		body = []byte(fmt.Sprintf("(error reading body: %v)", readErr))
	}
	truncated := len(body) > maxOutputBytes
	if truncated {
		body = trimPartialRune(body[:maxOutputBytes])
	}

	res.ExitCode = resp.StatusCode
	res.Output = fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body))
	if truncated {
		res.Output += truncationMarker
	}
	res.Ended = time.Now()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.Status = StatusSucceeded
	} else {
		res.Status = StatusFailed
	}
	return res
}

// toEnvSlice converts a map to a slice of "KEY=VALUE" strings.
// The output order is deterministic (sorted by key).
func toEnvSlice(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
