package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/config"
)

// TestServeRefusesWithoutMasterKey pins the fail-closed default: serve with no
// TEND_MASTER_KEY exits with an error naming the variable and the opt-out,
// prints nothing, and does not create the database. It used to start with
// alerting and the dashboard silently off.
func TestServeRefusesWithoutMasterKey(t *testing.T) {
	t.Setenv("TEND_ADDR", "127.0.0.1:0")
	cfg := tempConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{"serve"}, nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("serve without TEND_MASTER_KEY: err = nil, want a refusal")
	}
	for _, want := range []string{"TEND_MASTER_KEY", "--no-master-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %s", err, want)
		}
	}
	if stdout.Len() != 0 {
		t.Errorf("a refused serve printed %q", stdout.String())
	}
	if _, statErr := os.Stat(cfg.DSN); !os.IsNotExist(statErr) {
		t.Errorf("a refused serve must not create the database at %s (stat err = %v)", cfg.DSN, statErr)
	}
}

// TestServeRejectsInvalidMasterKey: a key that is set but malformed is an
// error, not a reason to fall back to running without one.
func TestServeRejectsInvalidMasterKey(t *testing.T) {
	t.Setenv("TEND_ADDR", "127.0.0.1:0")
	for name, key := range map[string]string{
		"not base64": "not base64!",
		"too short":  "c2hvcnQ=",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := tempConfig(t)
			cfg.MasterKey = key
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			err := cli.Run(ctx, cfg, []string{"serve", "--no-master-key"}, nil, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "TEND_MASTER_KEY") {
				t.Fatalf("serve with an invalid key: err = %v, want an error naming TEND_MASTER_KEY", err)
			}
		})
	}
}

// TestServeRejectsStrayArguments: serve takes flags only.
func TestServeRejectsStrayArguments(t *testing.T) {
	cfg := keyedConfig(t)
	var stdout, stderr bytes.Buffer
	if err := cli.Run(context.Background(), cfg, []string{"serve", "now"}, nil, &stdout, &stderr); err == nil {
		t.Fatal("serve now: err = nil, want an error for the stray argument")
	}
}

// syncBuffer is a bytes.Buffer that serve's goroutine can write while the test
// reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServe runs serve with args until the test ends, waits for /healthz,
// and returns the address and serve's stdout.
func startServe(t *testing.T, cfg config.Config, args ...string) (addr string, stdout *syncBuffer) {
	t.Helper()
	addr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	t.Setenv("TEND_ADDR", addr)
	ctx, cancel := context.WithCancel(context.Background())
	stdout = &syncBuffer{}
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- cli.Run(ctx, cfg, append([]string{"serve"}, args...), nil, stdout, &stderr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("serve did not stop within 5s")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return addr, stdout
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never answered /healthz; stderr: %s", stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestServeNoMasterKeyStartsWithAlertsAndDashboardOff: the opt-out starts the
// server, says loudly that alerts and the dashboard are off, and mounts no
// login page.
func TestServeNoMasterKeyStartsWithAlertsAndDashboardOff(t *testing.T) {
	addr, stdout := startServe(t, tempConfig(t), "--no-master-key")
	banner := stdout.String()
	if !strings.Contains(banner, "alerts OFF") || !strings.Contains(banner, "dashboard OFF") {
		t.Errorf("banner does not say alerts and dashboard are off: %q", banner)
	}
	resp, err := http.Get("http://" + addr + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /login without a key = %d, want 404", resp.StatusCode)
	}
}

// TestServeWithMasterKeyIgnoresOptOut: a key that is set is always used.
func TestServeWithMasterKeyIgnoresOptOut(t *testing.T) {
	_, stdout := startServe(t, keyedConfig(t), "--no-master-key")
	if banner := stdout.String(); !strings.Contains(banner, "alerts on") || !strings.Contains(banner, "dashboard on") {
		t.Errorf("with a key set, banner = %q, want alerts on and dashboard on", banner)
	}
}

// TestServeRefusesAnInvalidRetentionWindow: a retention value that is not a
// whole number of days stops serve before it opens the database.
func TestServeRefusesAnInvalidRetentionWindow(t *testing.T) {
	t.Setenv("TEND_ADDR", "127.0.0.1:0")
	t.Setenv("TEND_RETENTION_RUNS_DAYS", "90d")
	cfg := keyedConfig(t)
	var stdout, stderr bytes.Buffer
	err := cli.Run(context.Background(), cfg, []string{"serve"}, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "TEND_RETENTION_RUNS_DAYS") {
		t.Fatalf("serve with TEND_RETENTION_RUNS_DAYS=90d: err = %v, want an error naming the variable", err)
	}
	if _, statErr := os.Stat(cfg.DSN); !os.IsNotExist(statErr) {
		t.Errorf("a refused serve must not create the database (stat err = %v)", statErr)
	}
}

// TestServeBannerShowsRetention: the banner prints the windows in effect.
func TestServeBannerShowsRetention(t *testing.T) {
	t.Setenv("TEND_RETENTION_EVENTS_DAYS", "0")
	_, stdout := startServe(t, keyedConfig(t))
	want := "retention events kept forever, job runs 90d (default, up from 30d before v0.3.0; set TEND_RETENTION_RUNS_DAYS to change), deliveries 7d"
	if banner := stdout.String(); !strings.Contains(banner, want) {
		t.Errorf("banner = %q, want %q", banner, want)
	}
}

// TestServeRefusesAnInvalidDeliveryHold: like the retention windows, a hold
// that is not a whole number of minutes stops serve before it opens the
// database.
func TestServeRefusesAnInvalidDeliveryHold(t *testing.T) {
	t.Setenv("TEND_ADDR", "127.0.0.1:0")
	t.Setenv("TEND_DELIVERY_HOLD_MINUTES", "15m")
	cfg := keyedConfig(t)
	var stdout, stderr bytes.Buffer
	err := cli.Run(context.Background(), cfg, []string{"serve"}, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "TEND_DELIVERY_HOLD_MINUTES") {
		t.Fatalf("serve with TEND_DELIVERY_HOLD_MINUTES=15m: err = %v, want an error naming the variable", err)
	}
	if _, statErr := os.Stat(cfg.DSN); !os.IsNotExist(statErr) {
		t.Errorf("a refused serve must not create the database (stat err = %v)", statErr)
	}
}

// TestServeRefusesAnInvalidPingBodyLimit: TEND_PING_BODY_KB is checked like
// the other settings, before the database is opened, and doctor reports it.
func TestServeRefusesAnInvalidPingBodyLimit(t *testing.T) {
	t.Setenv("TEND_ADDR", "127.0.0.1:0")
	t.Setenv("TEND_PING_BODY_KB", "500")
	cfg := keyedConfig(t)
	var stdout, stderr bytes.Buffer
	err := cli.Run(context.Background(), cfg, []string{"serve"}, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "TEND_PING_BODY_KB") {
		t.Fatalf("serve with TEND_PING_BODY_KB=500: err = %v, want an error naming the variable", err)
	}
	if _, statErr := os.Stat(cfg.DSN); !os.IsNotExist(statErr) {
		t.Errorf("a refused serve must not create the database (stat err = %v)", statErr)
	}

	t.Setenv("TEND_PING_BODY_KB", "64")
	stdout.Reset()
	if err := cli.Run(context.Background(), cfg, []string{"doctor"}, nil, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "ping bodies:  first 64 kB of each kept") {
		t.Errorf("doctor does not report the ping body limit:\n%s", stdout.String())
	}
}

// TestServeRefusesInvalidAlertOutputLines: TEND_ALERT_OUTPUT_LINES is checked
// like the other settings, before the database is opened, and doctor
// reports it.
func TestServeRefusesInvalidAlertOutputLines(t *testing.T) {
	t.Setenv("TEND_ADDR", "127.0.0.1:0")
	t.Setenv("TEND_ALERT_OUTPUT_LINES", "51")
	cfg := keyedConfig(t)
	var stdout, stderr bytes.Buffer
	err := cli.Run(context.Background(), cfg, []string{"serve"}, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "TEND_ALERT_OUTPUT_LINES") {
		t.Fatalf("serve with TEND_ALERT_OUTPUT_LINES=51: err = %v, want an error naming the variable", err)
	}
	if _, statErr := os.Stat(cfg.DSN); !os.IsNotExist(statErr) {
		t.Errorf("a refused serve must not create the database (stat err = %v)", statErr)
	}

	for v, want := range map[string]string{"0": "alert output: none", "20": "alert output: last 20 lines"} {
		t.Setenv("TEND_ALERT_OUTPUT_LINES", v)
		stdout.Reset()
		if err := cli.Run(context.Background(), cfg, []string{"doctor"}, nil, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("TEND_ALERT_OUTPUT_LINES=%s: doctor does not say %q:\n%s", v, want, stdout.String())
		}
	}
}

// TestDoctorReportsAlertLinks: doctor says whether chat alerts carry links,
// and refuses to call a TEND_BASE_URL serve would leave out valid.
func TestDoctorReportsAlertLinks(t *testing.T) {
	cfg := keyedConfig(t)
	for v, want := range map[string]string{
		"":                           "alert links: none (TEND_BASE_URL is not set)",
		"https://tend.example/":      "alert links: https://tend.example/runs/<id>",
		"tend.example":               "alert links: none, TEND_BASE_URL is invalid:",
		"https://a:b@tend.example/x": "alert links: none, TEND_BASE_URL is invalid:",
	} {
		t.Setenv("TEND_BASE_URL", v)
		var stdout, stderr bytes.Buffer
		if err := cli.Run(context.Background(), cfg, []string{"doctor"}, nil, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("TEND_BASE_URL=%q: doctor does not say %q:\n%s", v, want, stdout.String())
		}
		if strings.Contains(stdout.String(), "a:b@") {
			t.Errorf("doctor printed the base URL's password:\n%s", stdout.String())
		}
	}
}
