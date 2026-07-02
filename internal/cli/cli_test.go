package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/config"
	"github.com/marsadhq/tend/internal/core"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// tempConfig returns a Config pointing at a fresh temp-dir SQLite database.
func tempConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	return config.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(dir, "tend_test.db"),
	}
}

// openTestStore opens an already-migrated SQLite store at dsn.
func openTestStore(t *testing.T, dsn string) *store.SQLiteStore {
	t.Helper()
	st, err := store.OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestRunCommandExecutesJob verifies that `tend run <name>` actually executes
// the job (i.e. DrainOnce runs the enqueued run), and that the result is
// reported as succeeded in stdout and recorded in the store.
func TestRunCommandExecutesJob(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	// Set up a job via `job add` so we know a run-able job exists.
	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "echo-test",
		"-type", "shell",
		"-command", "echo hello",
		"-cron", "* * * * *",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}
	t.Logf("job add stdout: %s", stdout.String())

	// Now run the job.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"run", "echo-test"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	t.Logf("run output: %s", out)

	if !strings.Contains(out, "succeeded") {
		t.Errorf("expected 'succeeded' in run output, got: %q", out)
	}

	// Also verify via the store directly.
	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "echo-test")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	runs, err := st.ListRuns(ctx, org.ID, j.ID, 1)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) == 0 {
		t.Fatal("expected at least one run, got none")
	}
	if runs[0].Status != jobs.StatusSucceeded {
		t.Errorf("run status = %q; want %q", runs[0].Status, jobs.StatusSucceeded)
	}
}

// TestSyncReconcilesFromFile writes a temp YAML file and exercises `tend sync`,
// asserting that the expected jobs are created.
func TestSyncReconcilesFromFile(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	// Write a minimal YAML config.
	yamlContent := `jobs:
  - name: sync-job-a
    type: shell
    command: "echo sync-a"
    cron: "0 1 * * *"
  - name: sync-job-b
    type: shell
    command: "echo sync-b"
    interval_seconds: 120
`
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "jobs.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{"sync", yamlPath}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("sync: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	t.Logf("sync output: %s", out)
	if !strings.Contains(out, "created=2") {
		t.Errorf("expected 'created=2' in sync output, got: %q", out)
	}

	// Verify via job list.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "list"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job list: %v\nstderr: %s", err, stderr.String())
	}
	listOut := stdout.String()
	t.Logf("job list output: %s", listOut)

	for _, name := range []string{"sync-job-a", "sync-job-b"} {
		if !strings.Contains(listOut, name) {
			t.Errorf("job list output does not contain %q:\n%s", name, listOut)
		}
	}

	// Second sync of the same file → updated=2, created=0, disabled=0.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"sync", yamlPath}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("sync (2nd): %v", err)
	}
	out2 := stdout.String()
	t.Logf("sync (2nd) output: %s", out2)
	if !strings.Contains(out2, "updated=2") {
		t.Errorf("expected 'updated=2' in second sync output, got: %q", out2)
	}
}

// TestVersionCommand verifies the version subcommand prints the version string.
func TestVersionCommand(t *testing.T) {
	cli.Version = "test-v1.2.3"
	var stdout, stderr bytes.Buffer
	// Version does not open the store so DSN can be anything.
	err := cli.Run(context.Background(), config.Config{Driver: "sqlite", DSN: ":memory:"}, []string{"version"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(stdout.String(), "test-v1.2.3") {
		t.Errorf("version output %q does not contain %q", stdout.String(), "test-v1.2.3")
	}
}

// TestSecretSetAndRun verifies that `secret set` stores a secret and a job
// referencing it via {{ secret.X }} can run successfully with a master key.
func TestSecretSetAndRun(t *testing.T) {
	// 32 zero bytes base64-encoded.
	masterKey := base64.StdEncoding.EncodeToString(make([]byte, 32))

	cfg := tempConfig(t)
	cfg.MasterKey = masterKey
	ctx := context.Background()

	// Store a secret value.
	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{"secret", "set", "my_token"},
		strings.NewReader("supersecret\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("secret set: %v\nstderr: %s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "supersecret") {
		t.Error("secret set: secret value was echoed in output (security violation)")
	}
	t.Logf("secret set output: %s", stdout.String())

	// Add a job using -env to pass the secret reference via the CLI flag.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "secret-job",
		"-type", "shell",
		"-command", `echo "value=$MY_TOKEN"`,
		"-cron", "* * * * *",
		"-env", "MY_TOKEN={{ secret.my_token }}",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	// Run the job via CLI.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"run", "secret-job"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run secret-job: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	t.Logf("run secret-job output: %s", out)
	if !strings.Contains(out, "succeeded") {
		t.Errorf("expected 'succeeded', got: %q", out)
	}
	// The plaintext secret must not appear in CLI output.
	if strings.Contains(out, "supersecret") {
		t.Error("plaintext secret value leaked into CLI output")
	}
}

// TestJobAddAcceptsNoSchedule asserts that `job add -name manual -command 'echo hi'`
// with NO -cron or -interval succeeds and creates a manual/on-demand job.
// TestHeartbeatShowAndPingURL verifies the ping-URL recovery commands:
// `heartbeat ping-url <name>` prints exactly <TEND_BASE_URL>/ping/<token>, and
// `heartbeat show <name>` prints name, status, period, grace, and the ping URL.
// This makes a config-as-code heartbeat's token recoverable without the DB.
func TestHeartbeatShowAndPingURL(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	t.Setenv("TEND_BASE_URL", "https://tend.example.com")

	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "add", "-name", "offsite-backup", "-period", "3600", "-grace", "300"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat add: %v\nstderr: %s", err, stderr.String())
	}
	addOut := stdout.String()
	const marker = "ping URL: "
	i := strings.Index(addOut, marker)
	if i < 0 {
		t.Fatalf("heartbeat add did not print a ping URL: %q", addOut)
	}
	wantURL := strings.TrimSpace(addOut[i+len(marker):])
	if !strings.HasPrefix(wantURL, "https://tend.example.com/ping/") {
		t.Fatalf("ping URL = %q, want https://tend.example.com/ping/<token>", wantURL)
	}

	// ping-url prints EXACTLY that URL (recoverable after the fact).
	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "ping-url", "offsite-backup"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat ping-url: %v\nstderr: %s", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != wantURL {
		t.Errorf("ping-url = %q, want %q", got, wantURL)
	}

	// show prints name, status, period, grace, and the ping URL.
	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "show", "offsite-backup"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat show: %v\nstderr: %s", err, stderr.String())
	}
	showOut := stdout.String()
	for _, want := range []string{"offsite-backup", "new", "3600", "300", wantURL} {
		if !strings.Contains(showOut, want) {
			t.Errorf("heartbeat show output missing %q; got:\n%s", want, showOut)
		}
	}

	// Unknown name errors clearly rather than printing an empty/garbage URL.
	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "ping-url", "nope"}, nil, &stdout, &stderr); err == nil {
		t.Errorf("heartbeat ping-url on unknown name: expected error, got nil (out=%q)", stdout.String())
	}
}

// TestDoctorReportsResolvedDBAndCounts verifies `tend doctor` surfaces the
// resolved driver + DB path, the org, the base URL, and resource counts, which
// is the fix for the silent CLI-vs-server DB mismatch (different TEND_DB).
func TestDoctorReportsResolvedDBAndCounts(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	t.Setenv("TEND_BASE_URL", "https://tend.example.com")

	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "add", "-name", "hb1", "-period", "60"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat add: %v\nstderr: %s", err, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"doctor"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("doctor: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"sqlite", cfg.DSN, "https://tend.example.com", "jobs=0", "heartbeats=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q; got:\n%s", want, out)
		}
	}
}

// TestHeartbeatHistory verifies `tend heartbeat history <name>` prints the
// missed/recovered transitions (newest first) with timestamps, and errors on an
// unknown name.
func TestHeartbeatHistory(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "add", "-name", "offsite-backup", "-period", "60"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat add: %v\nstderr: %s", err, stderr.String())
	}

	// Emit transition events into the same database.
	st := openTestStore(t, cfg.DSN)
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, ev := range []core.Event{
		{OrgID: org.ID, Type: "heartbeat.missed", Source: "heartbeat", Payload: "offsite-backup"},
		{OrgID: org.ID, Type: "heartbeat.recovered", Source: "heartbeat", Payload: "offsite-backup"},
	} {
		if _, err := st.EmitEvent(ctx, ev); err != nil {
			t.Fatalf("EmitEvent: %v", err)
		}
	}
	_ = st.Close()

	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "history", "offsite-backup"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("heartbeat history: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"heartbeat.missed", "heartbeat.recovered"} {
		if !strings.Contains(out, want) {
			t.Errorf("history output missing %q; got:\n%s", want, out)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"heartbeat", "history", "nope"}, nil, &stdout, &stderr); err == nil {
		t.Errorf("history on unknown name: expected error, got nil (out=%q)", stdout.String())
	}
}

func TestJobAddAcceptsNoSchedule(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "manual",
		"-command", "echo hi",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add (no schedule): %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "job created") {
		t.Errorf("expected 'job created' in output, got: %q", out)
	}
}

// TestJobListShowsManualJobAsNoneWithDashNextRun asserts that `job list` shows
// a no-schedule job with SCHEDULE=(none) and NEXT_RUN=-.
func TestJobListShowsManualJobAsNoneWithDashNextRun(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	// Create the manual job.
	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "manual",
		"-command", "echo hi",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	// List and inspect output.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "list"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job list: %v\nstderr: %s", err, stderr.String())
	}
	listOut := stdout.String()
	t.Logf("job list output:\n%s", listOut)

	if !strings.Contains(listOut, "(none)") {
		t.Errorf("expected schedule '(none)' in job list output, got:\n%s", listOut)
	}
	// NEXT_RUN column should be "-" (zero NextRunAt → dash).
	// The line with "manual" should contain "  -" as the last field.
	for _, line := range strings.Split(listOut, "\n") {
		if strings.Contains(line, "manual") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			last := fields[len(fields)-1]
			if last != "-" {
				t.Errorf("NEXT_RUN for manual job = %q; want \"-\"", last)
			}
		}
	}
}

// TestTickDoesNotEnqueueManualJob asserts that Runner.Tick does NOT enqueue a
// no-schedule job (NextRunAt is zero → excluded from DueJobs).
func TestTickDoesNotEnqueueManualJob(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	// Create a manual job via the CLI.
	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "manual-notick",
		"-command", "echo hi",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	// Open the store directly to get the job and run Tick.
	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "manual-notick")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// Confirm NextRunAt is zero - no auto-fire.
	if !j.NextRunAt.IsZero() {
		t.Fatalf("expected zero NextRunAt for manual job, got %v", j.NextRunAt)
	}

	r := jobs.NewRunner(st, jobs.NewExecutor(), nil, clock.RealClock{})
	if err := r.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	runs, err := st.ListRuns(ctx, org.ID, j.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("expected 0 runs after Tick for manual job, got %d", len(runs))
	}
}

// TestRunExecutesManualJobOnDemand asserts that `tend run manual` runs a
// no-schedule job on demand (it is run-able even without a schedule).
func TestRunExecutesManualJobOnDemand(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	// Create the manual job.
	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "manual-ondemand",
		"-command", "echo hi",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	// Run the job on demand.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"run", "manual-ondemand"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run manual-ondemand: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	t.Logf("run output: %s", out)
	if !strings.Contains(out, "succeeded") {
		t.Errorf("expected 'succeeded' in run output, got: %q", out)
	}
}

// TestJobAddEnvFlag verifies that `job add -env A=1 -env B=2` stores the
// correct Env map on the created job.
func TestJobAddEnvFlag(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "env-job",
		"-command", "echo hi",
		"-env", "A=1",
		"-env", "B=2",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "env-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	want := map[string]string{"A": "1", "B": "2"}
	if !reflect.DeepEqual(j.Env, want) {
		t.Errorf("job.Env = %v; want %v", j.Env, want)
	}
}

// TestJobAddEnvFlagBadEntry verifies that `-env bad` (no `=`) returns a clear error.
func TestJobAddEnvFlagBadEntry(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "bad-env-job",
		"-command", "echo hi",
		"-env", "badentry",
	}, nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for -env without =, got nil")
	}
	if !strings.Contains(err.Error(), "=") && !strings.Contains(stderr.String(), "=") {
		t.Errorf("error should mention missing '='; got err=%v stderr=%s", err, stderr.String())
	}

	// No job should have been created.
	st := openTestStore(t, cfg.DSN)
	if err2 := st.Migrate(ctx); err2 != nil {
		t.Fatalf("migrate: %v", err2)
	}
	org, err2 := st.BootstrapDefaultOrg(ctx)
	if err2 != nil {
		t.Fatalf("bootstrap: %v", err2)
	}
	_, err2 = st.GetJobByName(ctx, org.ID, "bad-env-job")
	if err2 == nil {
		t.Error("job should not have been created after bad -env entry")
	}
}

// TestJobAddEnvFlagValueContainsEquals verifies that `-env URL=k=v` parses as
// key "URL" → value "k=v" (split on first `=` only).
func TestJobAddEnvFlagValueContainsEquals(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "eq-job",
		"-command", "echo hi",
		"-env", "URL=k=v",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "eq-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	want := map[string]string{"URL": "k=v"}
	if !reflect.DeepEqual(j.Env, want) {
		t.Errorf("job.Env = %v; want %v", j.Env, want)
	}
}

// TestJobAddEnvFlagSecretRedaction verifies the end-to-end path:
// `job add -env MY={{ secret.s }}` then `run` shows `***` and NOT the secret value.
// This proves secret injection + redaction works via the CLI -env flag (not just the config path).
func TestJobAddEnvFlagSecretRedaction(t *testing.T) {
	masterKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cfg := tempConfig(t)
	cfg.MasterKey = masterKey
	ctx := context.Background()

	// Set the secret.
	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{"secret", "set", "s"},
		strings.NewReader("topsecret\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("secret set: %v\nstderr: %s", err, stderr.String())
	}

	// Add a job using -env with a secret reference via the CLI flag.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "redact-job",
		"-command", `echo "MY=$MY"`,
		"-env", "MY={{ secret.s }}",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	// Run the job.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"run", "redact-job"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run redact-job: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	t.Logf("run redact-job output: %s", out)

	// The plaintext secret must NOT appear in output.
	if strings.Contains(out, "topsecret") {
		t.Error("plaintext secret value 'topsecret' leaked into run output")
	}
	// The redaction marker *** MUST appear (proving inject+redact is working).
	if !strings.Contains(out, "***") {
		t.Errorf("output should contain *** (proving inject+redact via -env flag): %q", out)
	}
}

// TestJobAddRunAt verifies that `job add -run-at <future>` stores RunAt on the
// job and sets NextRunAt equal to run_at (since it is in the future).
func TestJobAddRunAt(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	runAtStr := future.Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "once",
		"-command", "echo hi",
		"-run-at", runAtStr,
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add -run-at: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "job created") {
		t.Errorf("expected 'job created' in output, got: %q", stdout.String())
	}

	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "once")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// RunAt must equal the parsed time.
	if !j.RunAt.Equal(future) {
		t.Errorf("job.RunAt = %v; want %v", j.RunAt, future)
	}
	// NextRunAt must equal run_at because it is in the future.
	if !j.NextRunAt.Equal(future) {
		t.Errorf("job.NextRunAt = %v; want %v (run_at)", j.NextRunAt, future)
	}
}

// TestJobAddRunAtBadFormat verifies that `-run-at not-a-time` returns a clear
// RFC3339 parse error and does not create a job.
func TestJobAddRunAtBadFormat(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "bad-runat-job",
		"-command", "echo hi",
		"-run-at", "not-a-time",
	}, nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for bad -run-at value, got nil")
	}
	if !strings.Contains(err.Error(), "RFC3339") && !strings.Contains(err.Error(), "run-at") && !strings.Contains(err.Error(), "run_at") {
		t.Errorf("error should mention RFC3339 or run-at; got: %v", err)
	}

	// No job should have been created.
	st := openTestStore(t, cfg.DSN)
	if err2 := st.Migrate(ctx); err2 != nil {
		t.Fatalf("migrate: %v", err2)
	}
	org, err2 := st.BootstrapDefaultOrg(ctx)
	if err2 != nil {
		t.Fatalf("bootstrap: %v", err2)
	}
	_, err2 = st.GetJobByName(ctx, org.ID, "bad-runat-job")
	if err2 == nil {
		t.Error("job should not have been created after bad -run-at format")
	}
}

// TestJobAddRunAtConflictsWithCron verifies that combining -cron and -run-at
// returns an "only one" error (schedCount > 1).
func TestJobAddRunAtConflictsWithCron(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "conflict-job",
		"-command", "echo hi",
		"-cron", "0 2 * * *",
		"-run-at", future,
	}, nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when both -cron and -run-at are set, got nil")
	}
	if !strings.Contains(err.Error(), "only one") {
		t.Errorf("error should say 'only one'; got: %v", err)
	}
}

// TestJobAddRunAtConflictsWithInterval verifies that combining -interval and
// -run-at returns an "only one" error (schedCount > 1).
func TestJobAddRunAtConflictsWithInterval(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "conflict-interval-job",
		"-command", "echo hi",
		"-interval", "60",
		"-run-at", future,
	}, nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when both -interval and -run-at are set, got nil")
	}
	if !strings.Contains(err.Error(), "only one") {
		t.Errorf("error should say 'only one'; got: %v", err)
	}
}

// TestJobAddRunAtPast verifies that a past -run-at creates a job with
// NextRunAt == zero (won't auto-fire since it already elapsed).
func TestJobAddRunAtPast(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "past-runat-job",
		"-command", "echo hi",
		"-run-at", past,
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add -run-at past: %v\nstderr: %s", err, stderr.String())
	}

	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "past-runat-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// NextRunAt must be zero - past run_at means it already elapsed, no auto-fire.
	if !j.NextRunAt.IsZero() {
		t.Errorf("expected zero NextRunAt for past run_at job, got %v", j.NextRunAt)
	}
}

// TestJobDisableAndEnable verifies that `job disable` sets Enabled=false and
// `job enable` sets Enabled=true, with job list reflecting yes/no accordingly.
func TestJobDisableAndEnable(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	// Create an enabled job.
	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "toggle-job",
		"-command", "echo hi",
		"-cron", "* * * * *",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	// Disable the job.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "disable", "toggle-job"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job disable: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "disabled") {
		t.Errorf("expected 'disabled' in disable output, got: %q", out)
	}

	// job list should show ENABLED no.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "list"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job list after disable: %v\nstderr: %s", err, stderr.String())
	}
	listOut := stdout.String()
	t.Logf("job list after disable:\n%s", listOut)
	for _, line := range strings.Split(listOut, "\n") {
		if strings.Contains(line, "toggle-job") {
			if !strings.Contains(line, "no") {
				t.Errorf("expected ENABLED=no for disabled job, line: %q", line)
			}
		}
	}

	// Also verify via store directly.
	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "toggle-job")
	if err != nil {
		t.Fatalf("get job after disable: %v", err)
	}
	if j.Enabled {
		t.Error("expected Enabled=false after job disable")
	}

	// Enable the job.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "enable", "toggle-job"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job enable: %v\nstderr: %s", err, stderr.String())
	}
	out = stdout.String()
	if !strings.Contains(out, "enabled") {
		t.Errorf("expected 'enabled' in enable output, got: %q", out)
	}

	// job list should show ENABLED yes.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "list"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job list after enable: %v\nstderr: %s", err, stderr.String())
	}
	listOut = stdout.String()
	t.Logf("job list after enable:\n%s", listOut)
	for _, line := range strings.Split(listOut, "\n") {
		if strings.Contains(line, "toggle-job") {
			if !strings.Contains(line, "yes") {
				t.Errorf("expected ENABLED=yes for enabled job, line: %q", line)
			}
		}
	}

	// Verify via store.
	j, err = st.GetJobByName(ctx, org.ID, "toggle-job")
	if err != nil {
		t.Fatalf("get job after enable: %v", err)
	}
	if !j.Enabled {
		t.Error("expected Enabled=true after job enable")
	}
}

// TestJobEnableDisablePreservesNextRunAt locks the behavior decision: enable and
// disable only flip Enabled and leave NextRunAt completely untouched, matching
// reconcileJobs which preserves NextRunAt when schedule is unchanged and non-zero.
func TestJobEnableDisablePreservesNextRunAt(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	// Create a cron job (so it gets a non-zero NextRunAt).
	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "nextrun-job",
		"-command", "echo hi",
		"-cron", "0 2 * * *",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	// Fetch the initial NextRunAt from the store.
	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
	before, err := st.GetJobByName(ctx, org.ID, "nextrun-job")
	if err != nil {
		t.Fatalf("get job before: %v", err)
	}
	if before.NextRunAt.IsZero() {
		t.Fatal("expected non-zero NextRunAt for cron job before disable")
	}
	originalNextRunAt := before.NextRunAt

	// Disable the job.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "disable", "nextrun-job"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job disable: %v\nstderr: %s", err, stderr.String())
	}

	// Verify NextRunAt is preserved after disable.
	afterDisable, err := st.GetJobByName(ctx, org.ID, "nextrun-job")
	if err != nil {
		t.Fatalf("get job after disable: %v", err)
	}
	if !afterDisable.NextRunAt.Equal(originalNextRunAt) {
		t.Errorf("NextRunAt changed on disable: got %v, want %v", afterDisable.NextRunAt, originalNextRunAt)
	}

	// Enable the job.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "enable", "nextrun-job"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job enable: %v\nstderr: %s", err, stderr.String())
	}

	// Verify NextRunAt is preserved after enable.
	afterEnable, err := st.GetJobByName(ctx, org.ID, "nextrun-job")
	if err != nil {
		t.Fatalf("get job after enable: %v", err)
	}
	if !afterEnable.NextRunAt.Equal(originalNextRunAt) {
		t.Errorf("NextRunAt changed on enable: got %v, want %v", afterEnable.NextRunAt, originalNextRunAt)
	}
}

// TestJobEnableDisableNotFound verifies that `job enable`/`job disable` with an
// unknown job name returns an error wrapping store.ErrNotFound.
func TestJobEnableDisableNotFound(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	for _, sub := range []string{"enable", "disable"} {
		t.Run(sub, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := cli.Run(ctx, cfg, []string{"job", sub, "does-not-exist"}, nil, &stdout, &stderr)
			if err == nil {
				t.Fatalf("job %s: expected error for unknown job, got nil", sub)
			}
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("job %s: expected err wrapping store.ErrNotFound; got: %v", sub, err)
			}
		})
	}
}

func TestJobEnableDisableMissingName(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	for _, sub := range []string{"enable", "disable"} {
		t.Run(sub, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := cli.Run(ctx, cfg, []string{"job", sub}, nil, &stdout, &stderr)
			if err == nil {
				t.Fatalf("job %s: expected error for missing job name, got nil", sub)
			}
			if !strings.Contains(err.Error(), "required") {
				t.Errorf("job %s: expected a 'name required' error; got: %v", sub, err)
			}
		})
	}
}

// TestJobRmRemovesJob verifies `job rm <name>` deletes the job so it no longer
// appears in `job list`.
func TestJobRmRemovesJob(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "rm-job",
		"-command", "echo hi",
		"-cron", "* * * * *",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	// Remove it.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "rm", "rm-job"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job rm: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "removed") {
		t.Errorf("expected 'removed' in rm output, got: %q", stdout.String())
	}

	// job list should no longer contain it.
	stdout.Reset()
	stderr.Reset()
	err = cli.Run(ctx, cfg, []string{"job", "list"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job list: %v\nstderr: %s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "rm-job") {
		t.Errorf("job list still contains rm-job after rm:\n%s", stdout.String())
	}
}

// TestJobRmNotFound verifies `job rm <unknown>` errors wrapping store.ErrNotFound.
func TestJobRmNotFound(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{"job", "rm", "does-not-exist"}, nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("job rm: expected error for unknown job, got nil")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("job rm: expected err wrapping store.ErrNotFound; got: %v", err)
	}
}

// TestJobRmMissingName verifies `job rm` with no name returns a clear error.
func TestJobRmMissingName(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{"job", "rm"}, nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("job rm: expected error for missing job name, got nil")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("job rm: expected a 'name required' error; got: %v", err)
	}
}

// TestSyncPruneDefaultPrintsDisabledNames verifies that syncing a partial config
// (default prune=true) disables the omitted job and prints the "disabled (absent
// from config)" line.
func TestSyncPruneDefaultPrintsDisabledNames(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	dir := t.TempDir()

	// Write a config with two jobs.
	fullYAML := `jobs:
  - name: prune-job-a
    type: shell
    command: "echo a"
    cron: "0 1 * * *"
  - name: prune-job-b
    type: shell
    command: "echo b"
    cron: "0 2 * * *"
`
	fullPath := filepath.Join(dir, "full.yaml")
	if err := os.WriteFile(fullPath, []byte(fullYAML), 0600); err != nil {
		t.Fatalf("write full yaml: %v", err)
	}

	// Seed both jobs.
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"sync", fullPath}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("sync (seed): %v\nstderr: %s", err, stderr.String())
	}

	// Write a partial config with only job-a.
	partialYAML := `jobs:
  - name: prune-job-a
    type: shell
    command: "echo a"
    cron: "0 1 * * *"
`
	partialPath := filepath.Join(dir, "partial.yaml")
	if err := os.WriteFile(partialPath, []byte(partialYAML), 0600); err != nil {
		t.Fatalf("write partial yaml: %v", err)
	}

	// Sync the partial config (default prune=true).
	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"sync", partialPath}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("sync (partial): %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	t.Logf("sync (partial) output: %s", out)

	// The disabled line must appear.
	if !strings.Contains(out, "disabled (absent from config): prune-job-b") {
		t.Errorf("expected 'disabled (absent from config): prune-job-b' in output, got: %q", out)
	}

	// Verify job-b is disabled in the store.
	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
	jobB, err := st.GetJobByName(ctx, org.ID, "prune-job-b")
	if err != nil {
		t.Fatalf("GetJobByName prune-job-b: %v", err)
	}
	if jobB.Enabled {
		t.Error("prune-job-b should be disabled after default-prune sync")
	}
}

// TestJobAddBodyFlagHTTPJob verifies that `job add -type http -url … -body '…'`
// stores HTTPBody correctly on the created job.
func TestJobAddBodyFlagHTTPJob(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "http-body-job",
		"-type", "http",
		"-url", "https://example.com",
		"-body", `{"k":"v"}`,
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "job created") {
		t.Errorf("expected 'job created' in output, got: %q", stdout.String())
	}

	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "http-body-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.HTTPBody != `{"k":"v"}` {
		t.Errorf("job.HTTPBody = %q; want %q", j.HTTPBody, `{"k":"v"}`)
	}
}

// TestJobAddBodyFlagDefaultEmpty verifies that `job add -type http -url …`
// with no -body flag stores HTTPBody == "" (default).
func TestJobAddBodyFlagDefaultEmpty(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "http-nobody-job",
		"-type", "http",
		"-url", "https://example.com",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add: %v\nstderr: %s", err, stderr.String())
	}

	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "http-nobody-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.HTTPBody != "" {
		t.Errorf("job.HTTPBody = %q; want empty string (no -body flag)", j.HTTPBody)
	}
}

// TestJobAddBodyFlagUnconditional verifies that `-body` is accepted on a shell
// job (stored unconditionally, mirroring config's http_body field which is
// likewise accepted on any job type). The body is inert for shell jobs but must
// not be rejected - CLI and config must stay consistent.
func TestJobAddBodyFlagUnconditional(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	err := cli.Run(ctx, cfg, []string{
		"job", "add",
		"-name", "shell-body-job",
		"-type", "shell",
		"-command", "echo hi",
		"-body", "x",
	}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("job add (shell + -body): %v\nstderr: %s", err, stderr.String())
	}

	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	j, err := st.GetJobByName(ctx, org.ID, "shell-body-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.HTTPBody != "x" {
		t.Errorf("job.HTTPBody = %q; want %q (unconditional store mirrors config)", j.HTTPBody, "x")
	}
}

// TestSyncPruneFalseSkipsDisable verifies that `sync -prune=false` leaves
// omitted jobs enabled and does NOT print the "disabled" line.
func TestSyncPruneFalseSkipsDisable(t *testing.T) {
	cfg := tempConfig(t)
	ctx := context.Background()
	dir := t.TempDir()

	// Write a config with two jobs.
	fullYAML := `jobs:
  - name: noprune-job-a
    type: shell
    command: "echo a"
    cron: "0 1 * * *"
  - name: noprune-job-b
    type: shell
    command: "echo b"
    cron: "0 2 * * *"
`
	fullPath := filepath.Join(dir, "full.yaml")
	if err := os.WriteFile(fullPath, []byte(fullYAML), 0600); err != nil {
		t.Fatalf("write full yaml: %v", err)
	}

	// Seed both jobs.
	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"sync", fullPath}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("sync (seed): %v\nstderr: %s", err, stderr.String())
	}

	// Write a partial config with only job-a.
	partialYAML := `jobs:
  - name: noprune-job-a
    type: shell
    command: "echo a"
    cron: "0 1 * * *"
`
	partialPath := filepath.Join(dir, "partial.yaml")
	if err := os.WriteFile(partialPath, []byte(partialYAML), 0600); err != nil {
		t.Fatalf("write partial yaml: %v", err)
	}

	// Sync the partial config with -prune=false.
	stdout.Reset()
	stderr.Reset()
	if err := cli.Run(ctx, cfg, []string{"sync", "-prune=false", partialPath}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("sync (no-prune): %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	t.Logf("sync (-prune=false) output: %s", out)

	// No "disabled" line should appear.
	if strings.Contains(out, "disabled (absent from config)") {
		t.Errorf("expected NO 'disabled' line with -prune=false, got: %q", out)
	}

	// Verify job-b is still enabled in the store.
	st := openTestStore(t, cfg.DSN)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
	jobB, err := st.GetJobByName(ctx, org.ID, "noprune-job-b")
	if err != nil {
		t.Fatalf("GetJobByName noprune-job-b: %v", err)
	}
	if !jobB.Enabled {
		t.Error("noprune-job-b should remain enabled after -prune=false sync")
	}
}
