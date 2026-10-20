package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/jobs"
)

// TestServeStartingDuringTendRunDoesNotRunTheJobTwice starts `tend serve`
// while `tend run` is still executing its job. serve's startup requeue used to
// put that run back in the queue, and serve's runner then executed the job a
// second time alongside the first.
func TestServeStartingDuringTendRunDoesNotRunTheJobTwice(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	dir := t.TempDir()
	release, starts := filepath.Join(dir, "release"), filepath.Join(dir, "starts")

	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{
		"job", "add", "-name", "once", "-type", "shell",
		"-command", "echo start >> '" + starts + "'; while [ ! -e '" + release + "' ]; do sleep 0.05; done",
	}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	var runOut, runErr bytes.Buffer
	runDone := make(chan error, 1)
	go func() { runDone <- cli.Run(ctx, cfg, []string{"run", "once"}, nil, &runOut, &runErr) }()
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })

	st := openTestStore(t, cfg.DSN)
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("tend run to claim its run", func() bool {
		running, err := st.ListRunningRuns(ctx)
		return err == nil && len(running) == 1
	})
	running, _ := st.ListRunningRuns(ctx)
	if !strings.HasPrefix(running[0].ClaimedBy, jobs.ManualClaimantPrefix) {
		t.Fatalf("tend run claimed its run as %q, want a %q claimant", running[0].ClaimedBy, jobs.ManualClaimantPrefix)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	t.Setenv("TEND_ADDR", addr)
	serveCtx, stopServe := context.WithCancel(ctx)
	serveDone := make(chan error, 1)
	var serveOut, serveErr bytes.Buffer
	go func() {
		serveDone <- cli.Run(serveCtx, cfg, []string{"serve", "--no-master-key"}, nil, &serveOut, &serveErr)
	}()
	waitFor("serve to answer /healthz", func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	// serve's idle workers poll the queue every second: give them the chance
	// to pick up a requeued run.
	time.Sleep(1500 * time.Millisecond)

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("tend run: %v\nstderr: %s", err, runErr.String())
	}
	stopServe()
	if err := <-serveDone; err != nil {
		t.Fatalf("serve: %v", err)
	}

	b, err := os.ReadFile(starts)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "start"); n != 1 {
		t.Fatalf("the job started %d times, want once", n)
	}
	if !strings.Contains(runOut.String(), "status=succeeded") {
		t.Errorf("tend run reported %q, want status=succeeded", runOut.String())
	}
}
