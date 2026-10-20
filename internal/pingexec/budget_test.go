package pingexec_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/pingexec"
)

// runTimed runs a command that records when it started under cfg against
// pingURL, with real waits between attempts, and returns how long after Run
// was called the command started, how long Run took, and the warnings.
func runTimed(t *testing.T, cfg pingexec.Config) (startedAfter, total time.Duration, code int, warnings string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "started")
	var logb bytes.Buffer
	cfg.Stdin, cfg.Stdout, cfg.Stderr, cfg.Log = strings.NewReader(""), io.Discard, io.Discard, &logb
	begin := time.Now()
	code = pingexec.Run(context.Background(), cfg, "sh", "-c", `date +%s%N > "$0"; exit 4`, marker)
	total = time.Since(begin)
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the command did not run: %v", err)
	}
	ns, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("marker %q: %v", b, err)
	}
	return time.Unix(0, ns).Sub(begin), total, code, logb.String()
}

// TestExecDoesNotHoldTheJobWhenTendRefuses: with the server refusing
// connections, the start ping gives up within its budget and the command runs
// anyway; the completion ping is bounded too.
func TestExecDoesNotHoldTheJobWhenTendRefuses(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens there now: connections are refused

	started, total, code, warnings := runTimed(t, pingexec.Config{
		PingURL: "http://" + addr + "/ping/tok", Start: true, Retries: 10,
		StartBudget: 1500 * time.Millisecond, FinishBudget: 1500 * time.Millisecond,
	})
	if code != 4 {
		t.Fatalf("exit code %d, want the command's 4", code)
	}
	if started > 2*time.Second || total > 4*time.Second {
		t.Fatalf("command started after %v, Run took %v; want the start ping given up within its 1.5 s budget", started, total)
	}
	if strings.Count(warnings, "giving up") != 2 {
		t.Fatalf("warnings %q, want both pings given up", warnings)
	}
}

// TestExecDoesNotHoldTheJobWhenTendIsBlackholed: a server that accepts the
// connection and never answers (a dropped route looks the same from here)
// costs the start ping its budget, not a request timeout per retry.
func TestExecDoesNotHoldTheJobWhenTendIsBlackholed(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() }) // read nothing, answer nothing
		}
	}()

	started, total, code, warnings := runTimed(t, pingexec.Config{
		PingURL: "http://" + l.Addr().String() + "/ping/tok", Start: true, Retries: 10, Timeout: 10 * time.Second,
		StartBudget: 1500 * time.Millisecond, FinishBudget: 1500 * time.Millisecond,
	})
	if code != 4 {
		t.Fatalf("exit code %d, want the command's 4", code)
	}
	if started > 2500*time.Millisecond || total > 5*time.Second {
		t.Fatalf("command started after %v, Run took %v; want each ping bounded by its 1.5 s budget", started, total)
	}
	if strings.Count(warnings, "giving up") != 2 {
		t.Fatalf("warnings %q, want both pings given up", warnings)
	}
}

// TestExecBudgetDefaults: the start ping may hold the job about 5 seconds,
// the completion ping about 20.
func TestExecBudgetDefaults(t *testing.T) {
	if pingexec.DefaultStartBudget != 5*time.Second || pingexec.DefaultFinishBudget != 20*time.Second {
		t.Fatalf("budgets %v / %v, want 5s / 20s", pingexec.DefaultStartBudget, pingexec.DefaultFinishBudget)
	}
}
