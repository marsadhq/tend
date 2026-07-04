package jobs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// ~1 MiB over the cap, from /dev/zero so it's fast.
	j := Job{Type: Shell, Command: "head -c $((11*1024*1024)) /dev/zero | tr '\\0' 'a'"}
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
