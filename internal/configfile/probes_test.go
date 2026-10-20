package configfile_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/tend/internal/configfile"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

const probesYAML = `jobs:
  - name: web-cert
    type: tls
    host: example.com
    interval_seconds: 21600
  - name: mail-cert
    type: tls
    host: mail.example.com
    port: 587
    server_name: smtp.example.com
    starttls: smtp
    ca_file: /etc/ssl/private-ca.pem
    warn_days: 7
    renew_after_fraction: 0.5
    restore: "not a drill"
  - name: lab-cert
    type: tls
    host: 192.0.2.10
    insecure_skip_verify: true
    warn_days: 0
    renew_after_fraction: 0
  - name: example-domain
    type: domain
    domain: Example.COM.
    cron: "0 6 * * *"
  - name: de-domain
    type: domain
    domain: example.de
    rdap_url: https://rdap.denic.de/
    warn_days: 60
  - name: licence
    type: expiry
    expires_at: 2027-01-31
  - name: node-key
    type: expiry
    expiry_command: "tailscale status --json | jq -r .Self.KeyExpiry"
    warn_days: 3
    env:
      TS_SOCKET: /run/tailscale/tailscaled.sock
  - name: db-restore-drill
    type: drill
    restore: 'sqlite3 /srv/app.db ".backup $TEND_SCRATCH/app.db"'
    checks:
      - sqlite3 "$TEND_SCRATCH/app.db" 'PRAGMA integrity_check' | grep -qx ok
      - test "$(sqlite3 "$TEND_SCRATCH/app.db" 'SELECT count(*) FROM users')" -ge 1
    cleanup: "true"
    scratch_dir: /var/tmp
    max_duration_seconds: 600
    timeout_seconds: 1800
    cron: "0 4 * * 0"
`

func TestParseProbesAndDrills(t *testing.T) {
	cfg, err := configfile.Parse([]byte(probesYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]struct {
		typ  jobs.JobType
		spec jobs.Spec
	}{
		"web-cert": {jobs.TLS, jobs.Spec{Host: "example.com", WarnDays: 14, RenewAfterFraction: 2.0 / 3}},
		"mail-cert": {jobs.TLS, jobs.Spec{Host: "mail.example.com", Port: 587, ServerName: "smtp.example.com", StartTLS: "smtp",
			CAFile: "/etc/ssl/private-ca.pem", WarnDays: 7, RenewAfterFraction: 0.5}},
		"lab-cert":       {jobs.TLS, jobs.Spec{Host: "192.0.2.10", InsecureSkipVerify: true}},
		"example-domain": {jobs.Domain, jobs.Spec{Domain: "example.com", WarnDays: 30}},
		"de-domain":      {jobs.Domain, jobs.Spec{Domain: "example.de", RDAPURL: "https://rdap.denic.de/", WarnDays: 60}},
		"licence":        {jobs.Expiry, jobs.Spec{ExpiresAt: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), WarnDays: 14}},
		"node-key":       {jobs.Expiry, jobs.Spec{ExpiryCommand: "tailscale status --json | jq -r .Self.KeyExpiry", WarnDays: 3}},
		"db-restore-drill": {jobs.Drill, jobs.Spec{
			Restore: `sqlite3 /srv/app.db ".backup $TEND_SCRATCH/app.db"`,
			Checks: []string{
				`sqlite3 "$TEND_SCRATCH/app.db" 'PRAGMA integrity_check' | grep -qx ok`,
				`test "$(sqlite3 "$TEND_SCRATCH/app.db" 'SELECT count(*) FROM users')" -ge 1`,
			},
			Cleanup: "true", ScratchDir: "/var/tmp", MaxDurationSeconds: 600,
		}},
	}
	if len(cfg.Jobs) != len(want) {
		t.Fatalf("parsed %d jobs, want %d", len(cfg.Jobs), len(want))
	}
	for _, j := range cfg.Jobs {
		w, ok := want[j.Name]
		if !ok {
			t.Fatalf("unexpected job %q", j.Name)
		}
		if j.Type != w.typ {
			t.Errorf("%s: type %q, want %q", j.Name, j.Type, w.typ)
		}
		if j.Spec == nil {
			t.Fatalf("%s: no spec", j.Name)
		}
		got := *j.Spec
		if !got.ExpiresAt.Equal(w.spec.ExpiresAt) {
			t.Errorf("%s: expires_at %v, want %v", j.Name, got.ExpiresAt, w.spec.ExpiresAt)
		}
		got.ExpiresAt, w.spec.ExpiresAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(got, w.spec) {
			t.Errorf("%s: spec\n got %+v\nwant %+v", j.Name, got, w.spec)
		}
		if !j.Enabled {
			t.Errorf("%s: not enabled", j.Name)
		}
	}
	// The job's own fields are read as for any job.
	for _, j := range cfg.Jobs {
		switch j.Name {
		case "db-restore-drill":
			if j.TimeoutSeconds != 1800 || j.Cron != "0 4 * * 0" {
				t.Errorf("drill: timeout %d cron %q", j.TimeoutSeconds, j.Cron)
			}
		case "node-key":
			if j.Env["TS_SOCKET"] != "/run/tailscale/tailscaled.sock" {
				t.Errorf("expiry job env = %v", j.Env)
			}
		}
	}
}

func TestParseShellAndHTTPHaveNoSpec(t *testing.T) {
	cfg, err := configfile.Parse([]byte(`jobs:
  - name: hello
    type: shell
    command: echo hi
    host: example.com
    warn_days: 3
  - name: ping
    type: http
    http_url: https://example.com
    restore: "true"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, j := range cfg.Jobs {
		if j.Spec != nil {
			t.Errorf("%s job %q got a spec: %+v", j.Type, j.Name, j.Spec)
		}
	}
}

func TestParseProbeValidation(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"tls without host", "type: tls", "host is required"},
		{"tls host with port", "type: tls\n    host: example.com:443", "port has its own setting"},
		{"tls starttls", "type: tls\n    host: example.com\n    starttls: pop3", "starttls"},
		{"tls fraction", "type: tls\n    host: example.com\n    renew_after_fraction: 1.5", "renew_after_fraction"},
		{"tls negative warn", "type: tls\n    host: example.com\n    warn_days: -1", "warn_days"},
		{"domain without domain", "type: domain", "domain is required"},
		{"domain not a domain", "type: domain\n    domain: localhost", "not a registered domain"},
		{"domain rdap_url", "type: domain\n    domain: example.com\n    rdap_url: rdap.example", "rdap_url"},
		{"expiry neither", "type: expiry", "expires_at or expiry_command"},
		{"expiry both", "type: expiry\n    expires_at: 2027-01-31\n    expiry_command: echo x", "not both"},
		{"expiry bad date", "type: expiry\n    expires_at: next tuesday", "expires_at"},
		{"drill without restore", "type: drill\n    checks: [\"true\"]", "restore is required"},
		{"drill empty check", "type: drill\n    restore: \"true\"\n    checks: [\"true\", \"\"]", "check 2 is empty"},
		{"drill relative scratch", "type: drill\n    restore: \"true\"\n    scratch_dir: tmp", "scratch_dir"},
		{"drill negative max", "type: drill\n    restore: \"true\"\n    max_duration_seconds: -5", "max_duration_seconds"},
		{"unknown type", "type: ftp", "must be shell, http, tls, domain, expiry or drill"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := configfile.Parse([]byte("jobs:\n  - name: probe-x\n    " + c.yaml + "\n"))
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), `"probe-x"`) {
				t.Fatalf("error %q does not name the job and %q", err, c.want)
			}
		})
	}
}

// TestReconcileProbes syncs probe and drill jobs into a store and back.
func TestReconcileProbes(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "tend.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))

	cfg, err := configfile.Parse([]byte(probesYAML))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configfile.Reconcile(ctx, st, org.ID, clk, nil, cfg, true); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, err := st.GetJobByName(ctx, org.ID, "mail-cert")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != jobs.TLS || got.Spec == nil || got.Spec.StartTLS != "smtp" || got.Spec.WarnDays != 7 {
		t.Fatalf("stored mail-cert = %+v / %+v", got, got.Spec)
	}
	drill, err := st.GetJobByName(ctx, org.ID, "db-restore-drill")
	if err != nil {
		t.Fatal(err)
	}
	if drill.NextRunAt.IsZero() || drill.Spec == nil || len(drill.Spec.Checks) != 2 {
		t.Fatalf("stored drill = %+v / %+v", drill, drill.Spec)
	}

	// A second sync with a changed setting updates the stored spec.
	cfg2, err := configfile.Parse([]byte(strings.Replace(probesYAML, "warn_days: 7", "warn_days: 21", 1)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := configfile.Reconcile(ctx, st, org.ID, clk, nil, cfg2, true)
	if err != nil {
		t.Fatalf("Reconcile again: %v", err)
	}
	if res.Updated != len(cfg2.Jobs) || res.Created != 0 {
		t.Fatalf("second sync: %+v", res)
	}
	got, err = st.GetJobByName(ctx, org.ID, "mail-cert")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.WarnDays != 21 {
		t.Fatalf("warn_days after re-sync = %d, want 21", got.Spec.WarnDays)
	}
}
