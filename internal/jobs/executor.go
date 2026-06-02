// Package jobs - executor.go runs a single Job (shell or HTTP) with timeout,
// retry-with-backoff, and captures combined output + exit code.
package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// killGraceDelay bounds how long cmd.Wait will block after the per-attempt
// context is cancelled: it gives I/O pipes a brief window to drain, then
// force-closes them so Run can never hang waiting on a stuck/forked child.
const killGraceDelay = 5 * time.Second

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

// Run executes j, retrying up to j.MaxRetries additional times on any
// non-succeeded outcome - including StatusTimedOut. Each failed attempt
// is followed by a backoff pause before the next attempt.
// Worst-case wall time ≈ MaxRetries*TimeoutSeconds + Σ(i² for i in 1..MaxRetries) seconds
// when using the default exponential backoff.
func (e *Executor) Run(ctx context.Context, j Job, env map[string]string) RunResult {
	maxAttempts := j.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var last RunResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		last = e.runOnce(ctx, j, env, attempt)
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

// runOnce executes j exactly once with a per-attempt timeout.
func (e *Executor) runOnce(ctx context.Context, j Job, env map[string]string, attempt int) RunResult {
	timeout := time.Duration(j.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()

	switch j.Type {
	case HTTP:
		return e.runOnceHTTP(cctx, j, attempt, started)
	default: // Shell or empty/default
		return e.runOnceShell(cctx, j, env, attempt, started)
	}
}

// runOnceShell runs the shell branch of runOnce.
func (e *Executor) runOnceShell(cctx context.Context, j Job, env map[string]string, attempt int, started time.Time) RunResult {
	cmd := exec.CommandContext(cctx, "sh", "-c", j.Command)
	// Make the shell the leader of its own process group and SIGKILL the whole
	// group on cancellation, so children the job forks are killed too.
	configureProcessGroup(cmd)
	// Backstop: bound Wait even if I/O pipes linger after the group is killed.
	cmd.WaitDelay = killGraceDelay
	// Overlay job env vars on top of the inherited OS environment so that
	// commands like "sleep" (found via PATH) continue to work.
	cmd.Env = append(os.Environ(), toEnvSlice(env)...)

	// TODO(M2): cap captured output size (io.LimitReader) to bound memory for chatty/runaway jobs.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()

	res := RunResult{
		Output:  buf.String(),
		Attempt: attempt,
		Started: started,
		Ended:   time.Now(),
	}

	switch {
	case cctx.Err() == context.DeadlineExceeded:
		res.Status = StatusTimedOut
	case err == nil:
		res.Status = StatusSucceeded
		res.ExitCode = 0
	default:
		res.Status = StatusFailed
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
		}
	}
	return res
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

	req, err := http.NewRequestWithContext(cctx, method, j.HTTPURL, bodyReader)
	if err != nil {
		return RunResult{
			Status:   StatusFailed,
			ExitCode: -1,
			Output:   err.Error(),
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
			res.Output = err.Error()
		}
		res.Ended = time.Now()
		return res
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		body = []byte(fmt.Sprintf("(error reading body: %v)", readErr))
	}

	res.ExitCode = resp.StatusCode
	res.Output = fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body))
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
