package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
)

// The two tests below pin that the commands which execute jobs hand their
// runner a logger. Nothing else notices when that assignment goes missing: the
// runner then works as before and logs nothing.

// TestServeGivesTheRunnerItsLogger starts `tend serve` on a database holding a
// run that an earlier process left 'running'. The runner requeues it at
// startup and says so, and that line must come out on serve's log.
func TestServeGivesTheRunnerItsLogger(t *testing.T) {
	t.Setenv("TEND_ADDR", "127.0.0.1:0")
	cfg := tempConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The run left behind: enqueued and claimed, with nobody executing it.
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"job", "add", "-name", "left-behind", "-type", "shell", "-command", "true"},
		nil, &stdout, &stderr); err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}
	st := openTestStore(t, cfg.DSN)
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("BootstrapDefaultOrg: %v", err)
	}
	job, err := st.GetJobByName(ctx, org.ID, "left-behind")
	if err != nil {
		t.Fatalf("GetJobByName: %v", err)
	}
	if _, err := st.EnqueueRun(ctx, org.ID, job.ID); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	if _, ok, err := st.ClaimRun(ctx, "runner"); err != nil || !ok {
		t.Fatalf("ClaimRun: ok=%v err=%v", ok, err)
	}

	// serve logs to os.Stderr, not to the writer it is passed: point that at a
	// file for as long as serve runs. The cli tests do not run in parallel.
	logPath := filepath.Join(t.TempDir(), "serve.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	realStderr := os.Stderr
	os.Stderr = logFile
	done := make(chan error, 1)
	go func() { done <- cli.Run(ctx, cfg, []string{"serve"}, nil, &stdout, &stderr) }()
	stop := func() error {
		cancel()
		err := <-done
		os.Stderr = realStderr
		return err
	}

	const want = "runner: requeued runs left running by an earlier process"
	deadline := time.Now().Add(10 * time.Second)
	for {
		logged, err := os.ReadFile(logPath)
		if err != nil {
			_ = stop()
			t.Fatal(err)
		}
		if strings.Contains(string(logged), want) {
			break
		}
		if time.Now().After(deadline) {
			_ = stop()
			t.Fatalf("serve did not log %q; its log:\n%s", want, logged)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stop(); err != nil {
		t.Errorf("serve: %v", err)
	}
}

// TestRunCommandGivesTheRunnerItsLogger fails a run from a second connection
// while `tend run` is still executing its job, the way a serve process reaping
// it would. The runner drops the late result and says so, and that line must
// come out on the command's stderr.
func TestRunCommandGivesTheRunnerItsLogger(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	release := filepath.Join(t.TempDir(), "release")
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{
		"job", "add", "-name", "late", "-type", "shell",
		"-command", "while [ ! -e '" + release + "' ]; do sleep 0.05; done",
	}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	var runOut, runErr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- cli.Run(ctx, cfg, []string{"run", "late"}, nil, &runOut, &runErr) }()
	finish := func() error {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Error(err)
		}
		return <-done
	}

	st := openTestStore(t, cfg.DSN)
	var running []jobs.Run
	deadline := time.Now().Add(10 * time.Second)
	for len(running) == 0 {
		var err error
		if running, err = st.ListRunningRuns(ctx); err != nil {
			_ = finish()
			t.Fatalf("ListRunningRuns: %v", err)
		}
		if len(running) == 0 && time.Now().After(deadline) {
			_ = finish()
			t.Fatal("the run was never claimed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	run := running[0]
	reaped, err := st.ReapStaleRun(ctx, run.ID, "reaped: worker lost", core.Event{
		OrgID: run.OrgID, Type: "run.failed", Source: "jobs.runner", Payload: `{"status":"failed"}`,
	})
	if err != nil || !reaped {
		_ = finish()
		t.Fatalf("ReapStaleRun: reaped=%v err=%v", reaped, err)
	}
	if err := finish(); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, runErr.String())
	}

	const want = "runner: discarded late result of a run that is no longer running"
	if !strings.Contains(runErr.String(), want) {
		t.Errorf("tend run did not log %q on stderr:\n%s", want, runErr.String())
	}
	if !strings.Contains(runOut.String(), "status=failed") {
		t.Errorf("tend run reported %q, want the state the run was left in (failed)", runOut.String())
	}
}
