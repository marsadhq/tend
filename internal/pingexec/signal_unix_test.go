//go:build unix

package pingexec_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/pingexec"
)

// TestExecSignalBeforeTheCommandStarts: a SIGTERM that arrives while the
// start ping is still being sent (a slow or unreachable server) stops the
// command from starting at all; the start ping is abandoned, the completion
// ping reports 143 and that is the exit status.
func TestExecSignalBeforeTheCommandStarts(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	gotStart := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/start") {
			select {
			case gotStart <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		_, _ = w.Write([]byte("OK"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	go func() {
		<-gotStart
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	marker := filepath.Join(t.TempDir(), "ran")
	begin := time.Now()
	code := pingexec.Run(context.Background(), pingexec.Config{
		PingURL: srv.URL + "/ping/tok", Start: true, Retries: 3,
		Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, Log: io.Discard,
		Sleep: func(context.Context, time.Duration) {},
	}, "sh", "-c", "touch "+marker)
	if code != 143 {
		t.Fatalf("exit code %d, want 143", code)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran although tend exec was stopped before it started")
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("Run took %v; the start ping should have been abandoned", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if last := paths[len(paths)-1]; last != "/ping/tok/143" {
		t.Fatalf("pings = %v, want /143 last", paths)
	}
}

// TestExecForwardsSignals: a SIGTERM to tend exec while the command runs is
// passed on to it, and its death by that signal is the exit status.
func TestExecForwardsSignals(t *testing.T) {
	f := newFakePings(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	begin := time.Now()
	code := pingexec.Run(context.Background(), pingexec.Config{
		PingURL: f.srv.URL + "/ping/tok", Start: true,
		Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, Log: io.Discard,
	}, "sleep", "10")
	if code != 143 || time.Since(begin) > 5*time.Second {
		t.Fatalf("exit code %d after %v, want 143 well before the sleep ends", code, time.Since(begin))
	}
	if hits := f.all(); hits[len(hits)-1].path != "/ping/tok/143" {
		t.Fatalf("pings = %+v, want /143 last", hits)
	}
}
