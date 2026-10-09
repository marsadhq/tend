package jobs

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// zeroBackoff disables inter-attempt delays so retry tests are instant.
func zeroBackoff(_ int) time.Duration { return 0 }

// Test 1: Shell success + capture
func TestExecutor_ShellSuccess(t *testing.T) {
	e := NewExecutor()
	j := Job{Type: Shell, Command: "echo hello"}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Errorf("expected StatusSucceeded, got %s", res.Status)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected ExitCode 0, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("expected output to contain 'hello', got %q", res.Output)
	}
	if res.Attempt != 1 {
		t.Errorf("expected Attempt 1, got %d", res.Attempt)
	}
	if res.Started.IsZero() {
		t.Error("expected non-zero Started time")
	}
	if res.Ended.IsZero() {
		t.Error("expected non-zero Ended time")
	}
}

// Test 2: Shell failure retries then fails
func TestExecutor_ShellFailureRetries(t *testing.T) {
	e := NewExecutor()
	e.Backoff = zeroBackoff
	j := Job{Type: Shell, Command: "exit 3", MaxRetries: 2}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusFailed {
		t.Errorf("expected StatusFailed, got %s", res.Status)
	}
	if res.ExitCode != 3 {
		t.Errorf("expected ExitCode 3, got %d", res.ExitCode)
	}
	if res.Attempt != 3 {
		t.Errorf("expected Attempt 3 (1 + 2 retries), got %d", res.Attempt)
	}
}

// Test 3: Timeout - also proves env inheritance (sleep must be found on PATH)
func TestExecutor_Timeout(t *testing.T) {
	e := NewExecutor()
	j := Job{Type: Shell, Command: "sleep 5", TimeoutSeconds: 1, MaxRetries: 0}
	start := time.Now()
	res := e.Run(context.Background(), j, nil)
	elapsed := time.Since(start)

	if res.Status != StatusTimedOut {
		t.Errorf("expected StatusTimedOut, got %s", res.Status)
	}
	// Should complete well under 5 seconds
	if elapsed > 4*time.Second {
		t.Errorf("expected timeout to fire within 4s, took %s", elapsed)
	}
}

// Test 4: Env injection
func TestExecutor_EnvInjection(t *testing.T) {
	e := NewExecutor()
	j := Job{Type: Shell, Command: "echo $TEND_TEST_VAR"}
	res := e.Run(context.Background(), j, map[string]string{"TEND_TEST_VAR": "injected-value"})

	if res.Status != StatusSucceeded {
		t.Errorf("expected StatusSucceeded, got %s", res.Status)
	}
	if !strings.Contains(res.Output, "injected-value") {
		t.Errorf("expected output to contain 'injected-value', got %q", res.Output)
	}
}

// Test 5: Retry then success (deterministic via temp-file marker)
func TestExecutor_RetryThenSuccess(t *testing.T) {
	e := NewExecutor()
	e.Backoff = zeroBackoff
	marker := filepath.Join(t.TempDir(), "m")
	cmd := "if [ -f " + marker + " ]; then exit 0; else touch " + marker + "; exit 1; fi"
	j := Job{Type: Shell, Command: cmd, MaxRetries: 2}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Errorf("expected StatusSucceeded, got %s", res.Status)
	}
	if res.Attempt != 2 {
		t.Errorf("expected Attempt 2, got %d", res.Attempt)
	}
}

// Test 6: HTTP success (200)
func TestExecutor_HTTPSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()

	e := NewExecutor()
	j := Job{Type: HTTP, HTTPURL: srv.URL}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Errorf("expected StatusSucceeded, got %s", res.Status)
	}
	if res.ExitCode != 200 {
		t.Errorf("expected ExitCode 200, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Output, "pong") {
		t.Errorf("expected output to contain 'pong', got %q", res.Output)
	}
}

// Test 7: HTTP failure (5xx)
func TestExecutor_HTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	e := NewExecutor()
	j := Job{Type: HTTP, HTTPURL: srv.URL}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusFailed {
		t.Errorf("expected StatusFailed, got %s", res.Status)
	}
	if res.ExitCode != 500 {
		t.Errorf("expected ExitCode 500, got %d", res.ExitCode)
	}
}

// Test 8: HTTP POST with body (echo server)
func TestExecutor_HTTPPostBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var buf strings.Builder
		_, _ = io.Copy(&buf, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(buf.String()))
	}))
	defer srv.Close()

	e := NewExecutor()
	j := Job{Type: HTTP, HTTPMethod: "POST", HTTPURL: srv.URL, HTTPBody: "ping"}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Errorf("expected StatusSucceeded, got %s", res.Status)
	}
	if !strings.Contains(res.Output, "ping") {
		t.Errorf("expected output to contain 'ping', got %q", res.Output)
	}
}

// Test 9: Timeout on attempt 1, success on attempt 2 - verifies that
// StatusTimedOut is treated as a retryable outcome by Run.
func TestExecutor_TimeoutThenSuccess(t *testing.T) {
	e := NewExecutor()
	e.Backoff = func(_ int) time.Duration { return 0 }
	marker := filepath.Join(t.TempDir(), "m")
	// Attempt 1: marker absent → touch marker + fork "sleep 5" and wait. Group-kill
	// terminates the forked grandchild at 1s → StatusTimedOut.
	// Attempt 2: marker present → exit 0 → StatusSucceeded.
	cmd := "if [ -f " + marker + " ]; then exit 0; else touch " + marker + "; sleep 5 & wait; fi"
	j := Job{Type: Shell, Command: cmd, TimeoutSeconds: 1, MaxRetries: 2}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Errorf("expected StatusSucceeded, got %s", res.Status)
	}
	if res.Attempt != 2 {
		t.Errorf("expected Attempt 2, got %d", res.Attempt)
	}
}

// Test 10: HTTP timeout - server sleeps longer than the job timeout.
func TestExecutor_HTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewExecutor()
	j := Job{Type: HTTP, HTTPURL: srv.URL, TimeoutSeconds: 1, MaxRetries: 0}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusTimedOut {
		t.Errorf("expected StatusTimedOut, got %s", res.Status)
	}
}

// Test 10b: HTTP transport error (connection refused) must not leak the
// request URL into job output — HTTPURL may embed a secret token in its
// query string or path, and job_runs.output is retained.
func TestExecutor_HTTPTransportError_RedactsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL + "/webhook?token=supersecret"
	srv.Close() // now refuses connections at the same host:port

	e := NewExecutor()
	j := Job{Type: HTTP, HTTPURL: deadURL, MaxRetries: 0}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusFailed {
		t.Fatalf("expected StatusFailed, got %s", res.Status)
	}
	if strings.Contains(res.Output, "supersecret") {
		t.Errorf("output leaked secret token: %q", res.Output)
	}
	if strings.Contains(res.Output, deadURL) {
		t.Errorf("output leaked full request URL: %q", res.Output)
	}
	if host := strings.TrimPrefix(srv.URL, "http://"); !strings.HasPrefix(res.Output, host+": ") {
		t.Errorf("output should still name the host %q for diagnosability: %q", host, res.Output)
	}
}

// Test 10c: a request that cannot even be built (before any network I/O) must
// not leak the URL either. The ways to get there: a URL net/url rejects, whose
// parse error quotes the whole URL and, for a bad escape, a bad character in
// the host or a bad port, the offending text once more; and an invalid method
// on a valid URL.
func TestExecutor_HTTPRequestBuildError_RedactsURL(t *testing.T) {
	tests := []struct {
		name       string
		job        Job
		wantPrefix string
		want       string // the whole output, when it is fixed text
	}{
		{
			name:       "unparseable URL",
			job:        Job{Type: HTTP, HTTPURL: "http://127.0.0.1:9/webhook?token=supersecret\x7f"},
			wantPrefix: "<url>: ",
		},
		{
			name:       "invalid method",
			job:        Job{Type: HTTP, HTTPMethod: "BAD METHOD", HTTPURL: "http://127.0.0.1:9/webhook?token=supersecret"},
			wantPrefix: "127.0.0.1:9: ",
		},
		{
			name:       "bad escape inside the token",
			job:        Job{Type: HTTP, HTTPURL: "http://127.0.0.1:9/webhook/super%secret"},
			wantPrefix: "<url>: ",
			want:       "<url>: invalid URL escape",
		},
		{
			name:       "bad character in the host",
			job:        Job{Type: HTTP, HTTPURL: "http://super{secret/webhook"},
			wantPrefix: "<url>: ",
			want:       "<url>: invalid character in host name",
		},
		{
			name:       "token where the port belongs",
			job:        Job{Type: HTTP, HTTPURL: "http://127.0.0.1:supersecret/webhook"},
			wantPrefix: "<url>: ",
			want:       "<url>: invalid URL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := NewExecutor().Run(context.Background(), tc.job, nil)

			if res.Status != StatusFailed {
				t.Fatalf("expected StatusFailed, got %s (output %q)", res.Status, res.Output)
			}
			if res.ExitCode != -1 {
				t.Errorf("expected ExitCode -1 for a request that was never sent, got %d", res.ExitCode)
			}
			if strings.Contains(res.Output, "supersecret") {
				t.Errorf("output leaked secret token: %q", res.Output)
			}
			if strings.Contains(res.Output, "/webhook") {
				t.Errorf("output leaked the request path: %q", res.Output)
			}
			if !strings.HasPrefix(res.Output, tc.wantPrefix) {
				t.Errorf("output = %q, want prefix %q", res.Output, tc.wantPrefix)
			}
			if tc.want != "" && res.Output != tc.want {
				t.Errorf("output = %q, want %q (no character of the URL quoted)", res.Output, tc.want)
			}
		})
	}
}

// Test 11: Parent context cancellation stops retrying
func TestExecutor_ParentCancelStopsRetries(t *testing.T) {
	e := NewExecutor()
	// Use a non-zero backoff so the cancel can interrupt it
	e.Backoff = func(_ int) time.Duration { return 500 * time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	j := Job{Type: Shell, Command: "exit 1", MaxRetries: 10}
	start := time.Now()
	_ = e.Run(ctx, j, nil)
	elapsed := time.Since(start)

	// Should complete well before 10*500ms = 5s
	if elapsed > 2*time.Second {
		t.Errorf("expected cancellation to stop retries quickly, took %s", elapsed)
	}
}

// Test 12: Timeout kills a forking job's whole process group. The command forks
// "sleep 30" into the background and waits on it; without process-group kill the
// orphaned grandchild keeps the captured-output pipe open and Run blocks ~30s.
// With the fix, the group is SIGKILLed at the 1s timeout and Run returns promptly.
func TestExecutor_TimeoutKillsForkingJob(t *testing.T) {
	e := NewExecutor()
	j := Job{Type: Shell, Command: "sleep 30 & wait", TimeoutSeconds: 1, MaxRetries: 0}
	start := time.Now()
	res := e.Run(context.Background(), j, nil)
	elapsed := time.Since(start)

	if res.Status != StatusTimedOut {
		t.Errorf("expected StatusTimedOut, got %s", res.Status)
	}
	// Proof the group was killed rather than waited on: must return far below the
	// forked child's 30s lifetime (and below timeout + killGraceDelay).
	if elapsed > 10*time.Second {
		t.Errorf("expected group-kill to bound wall time, took %s (child should have been killed)", elapsed)
	}
	t.Logf("TimeoutKillsForkingJob elapsed: %s", elapsed)
}

// Test 13: Shell output beyond the cap is discarded, the child still succeeds,
// and the kept output carries the truncation marker. Output below the cap is
// untouched (no marker).
func TestExecutor_ShellOutputCapped(t *testing.T) {
	e := NewExecutor()
	// 1 MiB over the cap, from /dev/zero so it's fast.
	j := Job{Type: Shell, Command: "head -c $((2*1024*1024)) /dev/zero | tr '\\0' 'a'"}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Fatalf("expected StatusSucceeded, got %s (output len %d)", res.Status, len(res.Output))
	}
	if len(res.Output) > maxOutputBytes+len(truncationMarker) {
		t.Errorf("output len = %d, want <= cap+marker (%d)", len(res.Output), maxOutputBytes+len(truncationMarker))
	}
	if !strings.HasSuffix(res.Output, truncationMarker) {
		t.Errorf("capped output missing truncation marker")
	}

	small := e.Run(context.Background(), Job{Type: Shell, Command: "echo small"}, nil)
	if strings.Contains(small.Output, truncationMarker) {
		t.Errorf("small output unexpectedly carries the truncation marker: %q", small.Output)
	}
}

// Test 14: HTTP response bodies beyond the cap are truncated with the marker;
// the run still reflects the HTTP status.
func TestExecutor_HTTPBodyCapped(t *testing.T) {
	big := strings.Repeat("b", maxOutputBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, big)
	}))
	defer srv.Close()

	e := NewExecutor()
	res := e.Run(context.Background(), Job{Type: HTTP, HTTPURL: srv.URL}, nil)

	if res.Status != StatusSucceeded {
		t.Fatalf("expected StatusSucceeded, got %s", res.Status)
	}
	if len(res.Output) > maxOutputBytes+len("HTTP 200\n")+len(truncationMarker) {
		t.Errorf("output len = %d, want capped", len(res.Output))
	}
	if !strings.HasSuffix(res.Output, truncationMarker) {
		t.Errorf("capped HTTP output missing truncation marker")
	}
}

// Test 15: Shell output beyond maxOutputBytes is capped and marked truncated.
func TestRun_Shell_OutputCapped(t *testing.T) {
	e := NewExecutor()
	j := Job{Type: Shell, Command: "head -c 2000000 /dev/zero | tr '\\0' 'a'"}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Fatalf("expected StatusSucceeded, got %s", res.Status)
	}
	if len(res.Output) > maxOutputBytes+len(truncationMarker) {
		t.Errorf("expected output capped at %d+marker, got %d bytes", maxOutputBytes, len(res.Output))
	}
	if !strings.HasSuffix(res.Output, truncationMarker) {
		t.Errorf("expected output to end with truncation marker, got suffix %q", res.Output[max(0, len(res.Output)-60):])
	}
}

// Test 16: Shell output under the cap is captured exactly, with no marker.
func TestRun_Shell_OutputUnderCap_NoMarker(t *testing.T) {
	e := NewExecutor()
	j := Job{Type: Shell, Command: "echo hello"}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Fatalf("expected StatusSucceeded, got %s", res.Status)
	}
	if res.Output != "hello\n" {
		t.Errorf("expected exact output %q, got %q", "hello\n", res.Output)
	}
	if strings.Contains(res.Output, truncationMarker) {
		t.Errorf("did not expect truncation marker in under-cap output: %q", res.Output)
	}
}

// Test 17: exit code / status are preserved even when output is truncated.
func TestRun_Shell_ExitCodePreservedWhenTruncated(t *testing.T) {
	e := NewExecutor()
	j := Job{Type: Shell, Command: "head -c 2000000 /dev/zero | tr '\\0' 'a'; exit 3"}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusFailed {
		t.Errorf("expected StatusFailed, got %s", res.Status)
	}
	if res.ExitCode != 3 {
		t.Errorf("expected ExitCode 3, got %d", res.ExitCode)
	}
	if !strings.HasSuffix(res.Output, truncationMarker) {
		t.Errorf("expected truncated output, got suffix %q", res.Output[max(0, len(res.Output)-60):])
	}
	// The cap is 1 MiB exactly: the kept output is that many bytes plus the
	// marker. Written as a literal so a changed constant cannot hide here.
	if want := 1<<20 + len(truncationMarker); len(res.Output) != want {
		t.Errorf("truncated output is %d bytes, want %d (1 MiB + marker)", len(res.Output), want)
	}
}

// Test 18: HTTP response body beyond maxOutputBytes is capped and marked truncated.
func TestRun_HTTP_BodyCapped(t *testing.T) {
	big := strings.Repeat("a", maxOutputBytes+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()

	e := NewExecutor()
	j := Job{Type: HTTP, HTTPURL: srv.URL}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Fatalf("expected StatusSucceeded, got %s", res.Status)
	}
	if len(res.Output) > maxOutputBytes+len(truncationMarker)+64 { // + "HTTP 200\n" header
		t.Errorf("expected output capped near %d, got %d bytes", maxOutputBytes, len(res.Output))
	}
	if !strings.HasSuffix(res.Output, truncationMarker) {
		t.Errorf("expected output to end with truncation marker, got suffix %q", res.Output[max(0, len(res.Output)-60):])
	}
}

// Test 19: HTTP response body under the cap is unchanged, with no marker.
func TestRun_HTTP_BodyUnderCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()

	e := NewExecutor()
	j := Job{Type: HTTP, HTTPURL: srv.URL}
	res := e.Run(context.Background(), j, nil)

	if res.Status != StatusSucceeded {
		t.Fatalf("expected StatusSucceeded, got %s", res.Status)
	}
	if strings.Contains(res.Output, truncationMarker) {
		t.Errorf("did not expect truncation marker in under-cap output: %q", res.Output)
	}
	if !strings.Contains(res.Output, "pong") {
		t.Errorf("expected output to contain 'pong', got %q", res.Output)
	}
}

// Test 20: maxRunDuration covers every attempt (timeout plus kill grace) and
// every backoff pause, so the stale-run reaper never mistakes a run that is
// still retrying for an orphan.
func TestMaxRunDuration(t *testing.T) {
	attempt := func(timeout time.Duration) time.Duration { return timeout + killGraceDelay }

	cases := []struct {
		name string
		e    *Executor
		job  Job
		want time.Duration
	}{
		{"no retries", NewExecutor(), Job{TimeoutSeconds: 600}, attempt(10 * time.Minute)},
		{"unset timeout uses the default", NewExecutor(), Job{}, attempt(DefaultTimeout)},
		{"negative timeout uses the default", NewExecutor(), Job{TimeoutSeconds: -5}, attempt(DefaultTimeout)},
		{"negative retries count as none", NewExecutor(), Job{TimeoutSeconds: 600, MaxRetries: -3}, attempt(10 * time.Minute)},
		{"one retry adds an attempt and a 1s pause", NewExecutor(), Job{TimeoutSeconds: 600, MaxRetries: 1},
			2*attempt(10*time.Minute) + 1*time.Second},
		{"three retries add 1s+4s+9s of pauses", NewExecutor(), Job{TimeoutSeconds: 60, MaxRetries: 3},
			4*attempt(time.Minute) + 14*time.Second},
		{"custom backoff is honoured", &Executor{Backoff: zeroBackoff}, Job{TimeoutSeconds: 60, MaxRetries: 3},
			4 * attempt(time.Minute)},
		{"absurd retry count saturates", NewExecutor(), Job{TimeoutSeconds: 600, MaxRetries: math.MaxInt32}, forever},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.e.maxRunDuration(c.job); got != c.want {
				t.Errorf("maxRunDuration = %s, want %s", got, c.want)
			}
		})
	}
}

// maxDurationSeconds is the most seconds a Duration can hold, a little over
// 292 years. It is an int64 because an int cannot hold it where int is 32 bits.
const maxDurationSeconds = math.MaxInt64 / int64(time.Second)

// asTimeout turns a number of seconds into a Job.TimeoutSeconds value. It skips
// the calling test where int is 32 bits: a timeout that large cannot be
// expressed there, so there is nothing to overflow.
func asTimeout(t *testing.T, secs int64) int {
	t.Helper()
	if strconv.IntSize < 64 {
		t.Skip("timeout_seconds cannot overflow a Duration where int is 32 bits")
	}
	return int(secs)
}

// Test 20b: maxRunDuration saturates for an enormous timeout as well. The
// first and the last case are boundaries that already worked. The others used
// to overflow: a timeout that only just fits wrapped into a negative limit once
// the kill grace was added, which the reaper read as "overdue as soon as the
// slack has passed", and the larger ones here wrapped into a negative timeout
// that was then mistaken for "unset".
func TestMaxRunDuration_EnormousTimeoutSaturates(t *testing.T) {
	cases := []struct {
		name    string
		seconds int64
		retries int
		want    time.Duration
	}{
		{"largest timeout that needs no saturating", maxDurationSeconds - 5, 0,
			time.Duration(maxDurationSeconds-5)*time.Second + killGraceDelay},
		{"timeout that fits until the kill grace is added", maxDurationSeconds, 0, forever},
		{"timeout one second past what a Duration holds", maxDurationSeconds + 1, 0, forever},
		{"ten digit timeout", 9999999999, 0, forever},
		{"largest possible timeout", math.MaxInt64, 0, forever},
		{"enormous timeout with retries", math.MaxInt64, 3, forever},
		{"large timeout that overflows across its retries", maxDurationSeconds / 2, 2, forever},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			job := Job{TimeoutSeconds: asTimeout(t, c.seconds), MaxRetries: c.retries}
			got := NewExecutor().maxRunDuration(job)
			if got != c.want {
				t.Errorf("maxRunDuration = %s, want %s", got, c.want)
			}
			if got <= 0 {
				t.Errorf("maxRunDuration = %d, a limit must be positive", got)
			}
		})
	}
}

// Test 20c: seconds is exact for ordinary values and saturates at both ends.
func TestSecondsSaturates(t *testing.T) {
	for in, want := range map[int]time.Duration{
		0: 0, 1: time.Second, -1: -time.Second, 7200: 2 * time.Hour, math.MaxInt32: math.MaxInt32 * time.Second,
	} {
		if got := seconds(in); got != want {
			t.Errorf("seconds(%d) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[int64]time.Duration{
		maxDurationSeconds:      time.Duration(maxDurationSeconds) * time.Second,
		maxDurationSeconds + 1:  forever,
		math.MaxInt64:           forever,
		-maxDurationSeconds:     -time.Duration(maxDurationSeconds) * time.Second,
		-maxDurationSeconds - 1: -forever,
		math.MinInt64:           -forever,
	} {
		if got := seconds(asTimeout(t, in)); got != want {
			t.Errorf("seconds(%d) = %d, want %d", in, got, want)
		}
	}
}

// Test 20d: a job with an enormous timeout runs like any other. The timeout
// used to overflow into a negative one, which ended every attempt as timed out
// before it had started.
func TestExecutor_EnormousTimeoutStillRuns(t *testing.T) {
	for _, secs := range []int64{maxDurationSeconds, maxDurationSeconds + 1, 9999999999, math.MaxInt64} {
		job := Job{Type: Shell, Command: "echo ok", TimeoutSeconds: asTimeout(t, secs)}
		res := NewExecutor().Run(context.Background(), job, nil)
		if res.Status != StatusSucceeded || res.Output != "ok\n" {
			t.Errorf("timeout_seconds=%d: status %s, output %q; want succeeded with the job's output", secs, res.Status, res.Output)
		}
	}
}

// Test 21: trimPartialRune removes only a sequence the cut left unfinished.
func TestTrimPartialRune(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"ascii", "abc", "abc"},
		{"complete 2-byte rune", "ab\u00e9", "ab\u00e9"},
		{"complete 3-byte rune", "ab\u20ac", "ab\u20ac"},
		{"complete 4-byte rune", "ab\U0001F600", "ab\U0001F600"},
		{"1 of 2 bytes", "ab\xc3", "ab"},
		{"1 of 3 bytes", "ab\xe2", "ab"},
		{"2 of 3 bytes", "ab\xe2\x82", "ab"},
		{"1 of 4 bytes", "ab\xf0", "ab"},
		{"2 of 4 bytes", "ab\xf0\x9f", "ab"},
		{"3 of 4 bytes", "ab\xf0\x9f\x98", "ab"},
		{"only a partial rune", "\xe2\x82", ""},
		{"invalid byte is not a partial rune", "ab\xff", "ab\xff"},
		{"stray continuation bytes are kept", "ab\x80\x80\x80", "ab\x80\x80\x80"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(trimPartialRune([]byte(c.in))); got != c.want {
				t.Errorf("trimPartialRune(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Test 22: truncation never splits a multi-byte character. Output that was
// valid UTF-8 stays valid after the cap for shell and HTTP jobs alike (a
// Postgres TEXT column rejects anything else), at every alignment of the cut.
func TestRun_TruncationKeepsOutputValidUTF8(t *testing.T) {
	// A 3-byte character, so the cap lands in a different place inside it as
	// the ASCII prefix grows.
	const euro = "\u20ac"
	for pad := 0; pad < len(euro); pad++ {
		body := strings.Repeat("x", pad) + strings.Repeat(euro, maxOutputBytes/len(euro)+16)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		path := filepath.Join(t.TempDir(), "out.txt")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		e := NewExecutor()
		for name, j := range map[string]Job{
			"shell": {Type: Shell, Command: "cat '" + path + "'"},
			"http":  {Type: HTTP, HTTPURL: srv.URL},
		} {
			res := e.Run(context.Background(), j, nil)
			if res.Status != StatusSucceeded {
				t.Fatalf("%s pad=%d: status %s, want succeeded", name, pad, res.Status)
			}
			if !strings.HasSuffix(res.Output, truncationMarker) {
				t.Fatalf("%s pad=%d: output was not truncated", name, pad)
			}
			if !utf8.ValidString(res.Output) {
				t.Errorf("%s pad=%d: truncated output is not valid UTF-8 (the cut split a character)", name, pad)
			}
			// At most one character's worth of bytes is given up to the cut.
			kept := len(strings.TrimPrefix(strings.TrimSuffix(res.Output, truncationMarker), "HTTP 200\n"))
			if kept > maxOutputBytes || kept <= maxOutputBytes-len(euro) {
				t.Errorf("%s pad=%d: kept %d bytes, want within one character of the %d byte cap", name, pad, kept, maxOutputBytes)
			}
		}
		srv.Close()
	}
}
