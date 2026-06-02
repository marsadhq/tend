package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/config"
)

// keyedConfig returns a temp-DB Config with a (zero-byte) master key set so the
// secrets box exists for channel encryption.
func keyedConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := tempConfig(t)
	cfg.MasterKey = base64.StdEncoding.EncodeToString(make([]byte, 32))
	return cfg
}

// TestChannelAddAndList adds a channel (config via stdin) and lists it.
func TestChannelAddAndList(t *testing.T) {
	cfg := keyedConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader(`{"webhook_url":"https://hooks.slack.com/services/T/B/x"}`)
	err := cli.Run(ctx, cfg, []string{"channel", "add", "-name", "ops-slack", "-type", "slack"},
		stdin, &stdout, &stderr)
	if err != nil {
		t.Fatalf("channel add: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ops-slack") {
		t.Errorf("channel add output %q missing channel name", stdout.String())
	}
	// The plaintext config must not be echoed.
	if strings.Contains(stdout.String(), "hooks.slack.com") {
		t.Error("channel add echoed config plaintext (security violation)")
	}

	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"channel", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("channel list: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ops-slack") || !strings.Contains(out, "slack") {
		t.Errorf("channel list output missing channel:\n%s", out)
	}
}

// TestRuleAddAndList adds a channel, a rule, then lists rules.
func TestRuleAddAndList(t *testing.T) {
	cfg := keyedConfig(t)
	ctx := context.Background()

	// Need a channel first.
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader(`{"url":"https://pager.example.com/h"}`)
	if err := cli.Run(ctx, cfg, []string{"channel", "add", "-name", "db-oncall", "-type", "webhook"},
		stdin, &stdout, &stderr); err != nil {
		t.Fatalf("channel add: %v\nstderr: %s", err, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"rule", "add", "-channel", "db-oncall", "-event", "run.failed"},
		nil, &stdout, &stderr); err != nil {
		t.Fatalf("rule add: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "run.failed") {
		t.Errorf("rule add output %q missing event", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"rule", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("rule list: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "db-oncall") || !strings.Contains(out, "run.failed") {
		t.Errorf("rule list output missing rule:\n%s", out)
	}
}

// TestHeartbeatAddAndList adds a heartbeat (asserting the ping URL is printed)
// and lists it.
func TestHeartbeatAddAndList(t *testing.T) {
	cfg := tempConfig(t) // no master key needed for heartbeats
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "add", "-name", "external-backup", "-period", "86400", "-grace", "3600"},
		nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat add: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "/ping/") {
		t.Errorf("heartbeat add output %q missing /ping/<token> URL", out)
	}

	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat list: %v\nstderr: %s", err, stderr.String())
	}
	listOut := stdout.String()
	if !strings.Contains(listOut, "external-backup") || !strings.Contains(listOut, "/ping/") {
		t.Errorf("heartbeat list output missing heartbeat or ping path:\n%s", listOut)
	}
}

// freePort returns an available localhost TCP port for the serve smoke test.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// TestServeFailsOnBindError verifies that serve returns a non-nil error promptly
// (within ~3s) when the configured port is already in use, instead of hanging
// until a SIGINT is delivered. This is a regression test for the serveCtx/cancelAll
// fix: without it, the runner and watcher goroutines would never stop.
func TestServeFailsOnBindError(t *testing.T) {
	cfg := keyedConfig(t)

	// Occupy the port for the lifetime of the test so ListenAndServe fails
	// immediately with EADDRINUSE.
	occupier, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	t.Cleanup(func() { occupier.Close() })
	addr := occupier.Addr().String()
	t.Setenv("TEND_ADDR", addr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		done <- cli.Run(ctx, cfg, []string{"serve"}, nil, &stdout, &stderr)
	}()

	select {
	case serveErr := <-done:
		if serveErr == nil {
			t.Error("serve returned nil error on bind failure, want non-nil")
		}
		t.Logf("serve returned error (expected): %v", serveErr)
	case <-time.After(3 * time.Second):
		cancel() // unblock the serve goroutine so the test doesn't leak
		t.Fatal("serve did not return within 3s of a bind failure - it is hanging")
	}
}

// TestServeSmoke starts `tend serve` in a goroutine, polls /healthz until 200,
// then cancels ctx and confirms serve returns promptly.
func TestServeSmoke(t *testing.T) {
	cfg := keyedConfig(t)
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	t.Setenv("TEND_ADDR", addr)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		done <- cli.Run(ctx, cfg, []string{"serve"}, nil, &stdout, &stderr)
	}()

	// Poll /healthz until 200 (timeout ~5s).
	healthURL := fmt.Sprintf("http://%s/healthz", addr)
	deadline := time.Now().Add(5 * time.Second)
	var got200 bool
	for time.Now().Before(deadline) {
		resp, err := http.Get(healthURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				got200 = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !got200 {
		cancel()
		<-done
		t.Fatalf("GET /healthz never returned 200 within 5s\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}

	// Startup line should mention the addr.
	if !strings.Contains(stdout.String(), addr) {
		t.Errorf("serve startup line missing addr %q:\n%s", addr, stdout.String())
	}

	// Cancel and confirm serve returns within ~3s.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned error on clean cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return within 3s of ctx cancel")
	}
}
