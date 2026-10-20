package cli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/cli"
)

const hcKey = "hcw_cli_secret_key_never_printed"

// hcAPI is a fake Healthchecks Management API with a simple, a cron and a
// paused check.
func hcAPI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/checks/" || r.Header.Get("X-Api-Key") != hcKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"checks": [
 {"name": "Filesystem Backup", "slug": "filesystem-backup", "grace": 600, "status": "up",
  "uuid": "5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278", "ping_url": "https://hc-ping.com/5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278", "timeout": 3600},
 {"name": "Database Backup", "slug": "database-backup", "grace": 1200, "status": "down",
  "uuid": "0b2f8a8e-4a46-4b0e-9a1c-3f0e2d8f1c55", "ping_url": "https://hc-ping.com/0b2f8a8e-4a46-4b0e-9a1c-3f0e2d8f1c55",
  "schedule": "15 5 * * *", "tz": "UTC"},
 {"name": "Old", "slug": "old", "grace": 60, "status": "paused", "timeout": 60,
  "uuid": "11111111-2222-4333-8444-555555555555"}
]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestImportHealthchecks: --dry-run prints the plan and writes nothing; the
// import creates heartbeats whose tokens are the checks' UUIDs, with a cron
// warning and the paused check skipped; a second import changes nothing; the
// API key is never printed.
func TestImportHealthchecks(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	t.Setenv("TEND_BASE_URL", "https://tend.example.com")
	srv := hcAPI(t)
	keyFile := filepath.Join(t.TempDir(), "hc.key")
	if err := os.WriteFile(keyFile, []byte(hcKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := cli.Run(ctx, cfg, args, strings.NewReader(""), &stdout, &stderr)
		out := stdout.String() + stderr.String()
		if strings.Contains(out, hcKey) || (err != nil && strings.Contains(err.Error(), hcKey)) {
			t.Fatalf("%v printed the API key:\n%s", args, out)
		}
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	imp := []string{"import", "healthchecks", "--api-key-file", keyFile, "--url", srv.URL}

	dry := run(append(imp, "--dry-run")...)
	for _, want := range []string{
		"create     filesystem-backup", "period 1h0m0s, grace 10m0s",
		"create     database-backup", "period 24h0m0s, grace 20m0s",
		"skip       old", "paused",
		`warning: "Database Backup" is a cron check`,
		"dry run: nothing was written",
	} {
		if !strings.Contains(dry, want) {
			t.Errorf("dry run output missing %q:\n%s", want, dry)
		}
	}
	if list := run("heartbeat", "list"); !strings.Contains(list, "(no heartbeats)") {
		t.Fatalf("the dry run wrote heartbeats:\n%s", list)
	}

	out := run(imp...)
	if !strings.Contains(out, "created 2, updated 0, unchanged 0, skipped 1") ||
		!strings.Contains(out, "https://hc-ping.com/<uuid> -> https://tend.example.com/ping/<uuid>") {
		t.Errorf("import output:\n%s", out)
	}
	show := run("heartbeat", "show", "filesystem-backup")
	for _, want := range []string{"period:    3600s", "grace:     600s", "ping url:  https://tend.example.com/ping/5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278"} {
		if !strings.Contains(show, want) {
			t.Errorf("imported heartbeat missing %q:\n%s", want, show)
		}
	}
	if show := run("heartbeat", "show", "database-backup"); !strings.Contains(show, "period:    86400s") {
		t.Errorf("cron check period:\n%s", show)
	}

	again := run(imp...)
	if !strings.Contains(again, "unchanged  filesystem-backup") || !strings.Contains(again, "created 0, updated 0, unchanged 2, skipped 1") {
		t.Errorf("second import:\n%s", again)
	}

	withPaused := run(append(imp, "--include-paused")...)
	if !strings.Contains(withPaused, "create     old") || !strings.Contains(withPaused, "Tend has no pause") {
		t.Errorf("--include-paused:\n%s", withPaused)
	}
}

// TestImportHealthchecksErrors: a missing or empty key file, a refused key and
// an unknown source are errors that never show the key.
func TestImportHealthchecksErrors(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	srv := hcAPI(t)
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.key")
	wrong := filepath.Join(dir, "wrong.key")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)
	_ = os.WriteFile(wrong, []byte("not-"+hcKey), 0o600)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"import"}, "healthchecks"},
		{[]string{"import", "cronitor"}, "healthchecks"},
		{[]string{"import", "healthchecks", "--url", srv.URL}, "--api-key-file"},
		{[]string{"import", "healthchecks", "--api-key-file", filepath.Join(dir, "missing.key"), "--url", srv.URL}, "missing.key"},
		{[]string{"import", "healthchecks", "--api-key-file", empty, "--url", srv.URL}, "empty"},
		{[]string{"import", "healthchecks", "--api-key-file", wrong, "--url", srv.URL}, "401"},
	} {
		var stdout, stderr bytes.Buffer
		err := cli.Run(ctx, cfg, c.args, strings.NewReader(""), &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want one mentioning %q", c.args, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), hcKey) {
			t.Errorf("%v: the error quotes the key", c.args)
		}
	}

	// The key can come on stdin.
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"import", "healthchecks", "--api-key-file", "-", "--url", srv.URL, "--dry-run"},
		strings.NewReader(hcKey+"\n"), &stdout, &stderr); err != nil {
		t.Fatalf("key on stdin: %v", err)
	}
}
