package cli_test

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/config"
	"github.com/marsadhq/tend/internal/jobs"
)

// addJob runs `tend job add` with args and returns the stored job.
func addJob(t *testing.T, dsn string, args ...string) (jobs.Job, error) {
	t.Helper()
	cfg := tempConfigAt(dsn)
	var stdout, stderr bytes.Buffer
	if err := cli.Run(context.Background(), cfg, append([]string{"job", "add"}, args...), nil, &stdout, &stderr); err != nil {
		return jobs.Job{}, err
	}
	st := openTestStore(t, dsn)
	org, err := st.BootstrapDefaultOrg(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for i, a := range args {
		if a == "-name" {
			name = args[i+1]
		}
	}
	return st.GetJobByName(context.Background(), org.ID, name)
}

// tempConfigAt is a Config for the SQLite database at dsn.
func tempConfigAt(dsn string) config.Config {
	return config.Config{Driver: "sqlite", DSN: dsn}
}

func TestJobAddProbesAndDrills(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "tend.db")
	cases := []struct {
		args []string
		typ  jobs.JobType
		spec jobs.Spec
	}{
		{[]string{"-name", "web-cert", "-type", "tls", "-host", "example.com", "-interval", "21600"},
			jobs.TLS, jobs.Spec{Host: "example.com", WarnDays: 14, RenewAfterFraction: 2.0 / 3}},
		{[]string{"-name", "mail-cert", "-type", "tls", "-host", "mail.example.com", "-port", "587", "-server-name", "smtp.example.com",
			"-starttls", "smtp", "-ca-file", "/etc/ssl/ca.pem", "-warn-days", "0", "-renew-after-fraction", "0"},
			jobs.TLS, jobs.Spec{Host: "mail.example.com", Port: 587, ServerName: "smtp.example.com", StartTLS: "smtp", CAFile: "/etc/ssl/ca.pem"}},
		{[]string{"-name", "lab-cert", "-type", "tls", "-host", "192.0.2.1", "-insecure-skip-verify", "-renew-after-fraction", "0.5"},
			jobs.TLS, jobs.Spec{Host: "192.0.2.1", InsecureSkipVerify: true, WarnDays: 14, RenewAfterFraction: 0.5}},
		{[]string{"-name", "domain", "-type", "domain", "-domain", "Example.COM", "-cron", "0 6 * * *"},
			jobs.Domain, jobs.Spec{Domain: "example.com", WarnDays: 30}},
		{[]string{"-name", "de", "-type", "domain", "-domain", "example.de", "-rdap-url", "https://rdap.denic.de/", "-warn-days", "45"},
			jobs.Domain, jobs.Spec{Domain: "example.de", RDAPURL: "https://rdap.denic.de/", WarnDays: 45}},
		{[]string{"-name", "licence", "-type", "expiry", "-expires-at", "2027-01-31"},
			jobs.Expiry, jobs.Spec{ExpiresAt: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), WarnDays: 14}},
		{[]string{"-name", "node-key", "-type", "expiry", "-expiry-command", "tailscale status --json | jq -r .Self.KeyExpiry", "-warn-days", "3"},
			jobs.Expiry, jobs.Spec{ExpiryCommand: "tailscale status --json | jq -r .Self.KeyExpiry", WarnDays: 3}},
		{[]string{"-name", "drill", "-type", "drill", "-restore", `cp /b/app.db "$TEND_SCRATCH/"`,
			"-check", `sqlite3 "$TEND_SCRATCH/app.db" 'PRAGMA integrity_check'`, "-check", "true",
			"-cleanup", "true", "-scratch-dir", "/var/tmp", "-max-duration", "600", "-timeout", "1800"},
			jobs.Drill, jobs.Spec{Restore: `cp /b/app.db "$TEND_SCRATCH/"`,
				Checks:  []string{`sqlite3 "$TEND_SCRATCH/app.db" 'PRAGMA integrity_check'`, "true"},
				Cleanup: "true", ScratchDir: "/var/tmp", MaxDurationSeconds: 600}},
	}
	for _, c := range cases {
		j, err := addJob(t, dsn, c.args...)
		if err != nil {
			t.Fatalf("job add %v: %v", c.args, err)
		}
		if j.Type != c.typ || j.Spec == nil {
			t.Fatalf("job add %v: type %q spec %v", c.args, j.Type, j.Spec)
		}
		got, want := *j.Spec, c.spec
		if !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Errorf("%s: expires_at %v, want %v", j.Name, got.ExpiresAt, want.ExpiresAt)
		}
		got.ExpiresAt, want.ExpiresAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: spec\n got %+v\nwant %+v", j.Name, got, want)
		}
	}

	// A shell job still has no settings, whatever probe flags it is given.
	j, err := addJob(t, dsn, "-name", "hello", "-command", "echo hi", "-host", "example.com", "-warn-days", "5")
	if err != nil {
		t.Fatal(err)
	}
	if j.Spec != nil {
		t.Fatalf("shell job got a spec: %+v", j.Spec)
	}
}

func TestJobAddRefusesBadProbes(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-type", "tls"}, "host is required"},
		{[]string{"-type", "tls", "-host", "example.com:443"}, "port has its own setting"},
		{[]string{"-type", "tls", "-host", "example.com", "-renew-after-fraction", "2"}, "renew_after_fraction"},
		{[]string{"-type", "domain", "-domain", "localhost"}, "not a registered domain"},
		{[]string{"-type", "expiry"}, "expires_at or expiry_command"},
		{[]string{"-type", "expiry", "-expires-at", "soon"}, "-expires-at"},
		{[]string{"-type", "expiry", "-expires-at", "2027-01-31", "-expiry-command", "echo x"}, "not both"},
		{[]string{"-type", "drill"}, "restore is required"},
		{[]string{"-type", "drill", "-restore", "true", "-scratch-dir", "tmp"}, "scratch_dir"},
		{[]string{"-type", "drill", "-restore", "true", "-max-duration", "-1"}, "max_duration"},
		{[]string{"-type", "ftp"}, "unknown type"},
		{[]string{"-type", "drill", "-restore", "true", "-command", `cp x "$TEND_SCRATCH"`}, "-command is not used by drill jobs"},
		{[]string{"-type", "expiry", "-expires-at", "2027-01-31", "-command", "date"}, "-expiry-command"},
		{[]string{"-type", "tls", "-host", "example.com", "-url", "https://example.com"}, "-url is not used by tls jobs"},
	}
	for _, c := range cases {
		dsn := filepath.Join(t.TempDir(), "tend.db")
		_, err := addJob(t, dsn, append([]string{"-name", "bad"}, c.args...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("job add %v: err = %v, want one about %q", c.args, err, c.want)
			continue
		}
		var stdout, stderr bytes.Buffer
		if err := cli.Run(context.Background(), tempConfigAt(dsn), []string{"job", "list"}, nil, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stdout.String(), "bad") {
			t.Errorf("job add %v stored the refused job:\n%s", c.args, stdout.String())
		}
	}
}

// TestRunProbeInline runs an expiry job with `tend run`, which prints the
// probe's verdict and fails the run when the date is near.
func TestRunProbeInline(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "tend.db")
	soon := time.Now().Add(5 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := addJob(t, dsn, "-name", "token", "-type", "expiry", "-expires-at", soon); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := cli.Run(context.Background(), tempConfigAt(dsn), []string{"run", "token"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "run: status=failed exit_code=1") || !strings.Contains(out, "FAIL: expires in 4 days") {
		t.Fatalf("run output:\n%s", out)
	}
}

func TestDoctorReportsProbesAndDrills(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "tend.db")
	missing := filepath.Join(t.TempDir(), "nope")
	for _, args := range [][]string{
		{"-name", "cert", "-type", "tls", "-host", "example.com", "-ca-file", filepath.Join(missing, "ca.pem")},
		{"-name", "drill-ok", "-type", "drill", "-restore", "true", "-scratch-dir", t.TempDir()},
		{"-name", "drill-bad", "-type", "drill", "-restore", "true", "-scratch-dir", missing},
		{"-name", "hello", "-command", "true"},
	} {
		if _, err := addJob(t, dsn, args...); err != nil {
			t.Fatalf("job add %v: %v", args, err)
		}
	}
	var stdout, stderr bytes.Buffer
	if err := cli.Run(context.Background(), tempConfigAt(dsn), []string{"doctor"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"job types:  drill=2 shell=1 tls=1",
		`warning: job "cert": ca_file`,
		`warning: job "drill-bad": scratch_dir`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"drill-ok"`) {
		t.Errorf("doctor warned about a drill whose scratch_dir is fine:\n%s", out)
	}
}
