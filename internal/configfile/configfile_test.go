package configfile_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/configfile"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// TestParseMapsYAMLToJobs verifies that parsing testdata/jobs.yaml produces
// the two expected jobs with all fields mapped correctly.
func TestParseMapsYAMLToJobs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "jobs.yaml"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	cfg, err := configfile.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	parsed := cfg.Jobs
	if len(parsed) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(parsed))
	}

	// nightly-backup
	j0 := parsed[0]
	if j0.Name != "nightly-backup" {
		t.Errorf("job[0].Name = %q; want %q", j0.Name, "nightly-backup")
	}
	if j0.Type != jobs.Shell {
		t.Errorf("job[0].Type = %q; want %q", j0.Type, jobs.Shell)
	}
	if j0.Command != "restic backup /data" {
		t.Errorf("job[0].Command = %q; want %q", j0.Command, "restic backup /data")
	}
	if j0.Cron != "0 3 * * *" {
		t.Errorf("job[0].Cron = %q; want %q", j0.Cron, "0 3 * * *")
	}
	if j0.TimeoutSeconds != 1800 {
		t.Errorf("job[0].TimeoutSeconds = %d; want 1800", j0.TimeoutSeconds)
	}
	if j0.MaxRetries != 2 {
		t.Errorf("job[0].MaxRetries = %d; want 2", j0.MaxRetries)
	}
	if !j0.Enabled {
		t.Error("job[0].Enabled = false; want true (omitted → default true)")
	}
	// Secret ref must be verbatim - NOT resolved here.
	if got := j0.Env["RESTIC_REPOSITORY"]; got != "{{ secret.restic_repo }}" {
		t.Errorf("job[0].Env[RESTIC_REPOSITORY] = %q; want %q", got, "{{ secret.restic_repo }}")
	}

	// health-poll
	j1 := parsed[1]
	if j1.Name != "health-poll" {
		t.Errorf("job[1].Name = %q; want %q", j1.Name, "health-poll")
	}
	if j1.Type != jobs.HTTP {
		t.Errorf("job[1].Type = %q; want %q", j1.Type, jobs.HTTP)
	}
	if j1.HTTPURL != "https://example.com/health" {
		t.Errorf("job[1].HTTPURL = %q; want %q", j1.HTTPURL, "https://example.com/health")
	}
	if j1.HTTPMethod != "GET" {
		t.Errorf("job[1].HTTPMethod = %q; want %q", j1.HTTPMethod, "GET")
	}
	if j1.IntervalSeconds != 300 {
		t.Errorf("job[1].IntervalSeconds = %d; want 300", j1.IntervalSeconds)
	}
	if !j1.Enabled {
		t.Error("job[1].Enabled = false; want true")
	}
}

// TestParseValidation checks that invalid YAML configs produce clear errors.
func TestParseValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "two schedules",
			yaml: `jobs:
  - name: two-sched
    type: shell
    command: echo hi
    cron: "* * * * *"
    interval_seconds: 60`,
			wantErr: "two-sched",
		},
		{
			name: "empty name",
			yaml: `jobs:
  - name: ""
    type: shell
    command: echo hi
    cron: "* * * * *"`,
			wantErr: "name",
		},
		{
			name: "unknown type",
			yaml: `jobs:
  - name: bad-type
    type: ftp
    command: echo hi
    cron: "* * * * *"`,
			wantErr: "bad-type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := configfile.Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.wantErr != "" {
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err.Error(), tc.wantErr)
				}
			}
		})
	}
}

// TestReconcileCreateUpdateDisable exercises the three reconcile paths:
// create → NextRunAt non-zero (the scheduling contract), update (ID preserved),
// and disable (job absent from config → Enabled=false).
func TestReconcileCreateUpdateDisable(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "tend_test.db")

	st, err := store.OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}

	// Fixed reference time: a Wednesday 2024-01-03 12:00:00 UTC.
	refTime := time.Date(2024, 1, 3, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(refTime)

	jobA := jobs.Job{
		Name:           "job-a",
		Type:           jobs.Shell,
		Command:        "echo hello",
		Cron:           "0 3 * * *",
		TimeoutSeconds: 60,
		Enabled:        true,
	}

	// --- Pass 1: create ---
	result, err := configfile.Reconcile(ctx, st, org.ID, clk, nil, configfile.Config{Jobs: []jobs.Job{jobA}}, true)
	if err != nil {
		t.Fatalf("Reconcile (create): %v", err)
	}
	if result.Created != 1 {
		t.Errorf("Created = %d; want 1", result.Created)
	}
	if result.Updated != 0 {
		t.Errorf("Updated = %d; want 0 (pass 1)", result.Updated)
	}
	if result.Disabled != 0 {
		t.Errorf("Disabled = %d; want 0 (pass 1)", result.Disabled)
	}

	// THE CRITICAL ASSERTION: NextRunAt must be non-zero after create.
	stored, err := st.GetJobByName(ctx, org.ID, "job-a")
	if err != nil {
		t.Fatalf("GetJobByName after create: %v", err)
	}
	if stored.NextRunAt.IsZero() {
		t.Fatal("SCHEDULING CONTRACT VIOLATED: NextRunAt is zero after Reconcile create - job will never fire")
	}
	t.Logf("NextRunAt after create: %v", stored.NextRunAt)
	savedID := stored.ID
	if savedID == 0 {
		t.Fatal("stored job ID is 0 after create")
	}

	// --- Pass 2: update (changed command) ---
	jobAUpdated := jobA
	jobAUpdated.Command = "echo world"
	result, err = configfile.Reconcile(ctx, st, org.ID, clk, nil, configfile.Config{Jobs: []jobs.Job{jobAUpdated}}, true)
	if err != nil {
		t.Fatalf("Reconcile (update): %v", err)
	}
	if result.Created != 0 {
		t.Errorf("Created = %d; want 0 (pass 2)", result.Created)
	}
	if result.Updated != 1 {
		t.Errorf("Updated = %d; want 1 (pass 2)", result.Updated)
	}
	if result.Disabled != 0 {
		t.Errorf("Disabled = %d; want 0 (pass 2)", result.Disabled)
	}

	storedUpdated, err := st.GetJobByName(ctx, org.ID, "job-a")
	if err != nil {
		t.Fatalf("GetJobByName after update: %v", err)
	}
	if storedUpdated.ID != savedID {
		t.Errorf("ID changed after update: was %d, now %d", savedID, storedUpdated.ID)
	}
	if storedUpdated.Command != "echo world" {
		t.Errorf("Command not updated: got %q; want %q", storedUpdated.Command, "echo world")
	}

	// --- Pass 3: disable (empty list) ---
	result, err = configfile.Reconcile(ctx, st, org.ID, clk, nil, configfile.Config{Jobs: []jobs.Job{}}, true)
	if err != nil {
		t.Fatalf("Reconcile (disable): %v", err)
	}
	if result.Created != 0 {
		t.Errorf("Created = %d; want 0 (pass 3)", result.Created)
	}
	if result.Updated != 0 {
		t.Errorf("Updated = %d; want 0 (pass 3)", result.Updated)
	}
	if result.Disabled != 1 {
		t.Errorf("Disabled = %d; want 1 (pass 3)", result.Disabled)
	}

	storedDisabled, err := st.GetJobByName(ctx, org.ID, "job-a")
	if err != nil {
		t.Fatalf("GetJobByName after disable: %v", err)
	}
	if storedDisabled.Enabled {
		t.Error("job-a should be disabled after empty reconcile")
	}
}

// TestParseNoScheduleJobIsAccepted asserts that a job spec with no schedule
// (no cron, no interval_seconds, no run_at) parses without error - it is a
// valid manual/on-demand job.
func TestParseNoScheduleJobIsAccepted(t *testing.T) {
	yaml := `jobs:
  - name: manual-job
    type: shell
    command: echo hi`
	cfg, err := configfile.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: expected no error for no-schedule job, got: %v", err)
	}
	if len(cfg.Jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(cfg.Jobs))
	}
	if cfg.Jobs[0].Name != "manual-job" {
		t.Errorf("job name = %q; want %q", cfg.Jobs[0].Name, "manual-job")
	}
}

// TestParseTwoSchedulesStillErrors asserts that a job spec with two schedules
// is still rejected.
func TestParseTwoSchedulesStillErrors(t *testing.T) {
	yaml := `jobs:
  - name: two-sched
    type: shell
    command: echo hi
    cron: "* * * * *"
    interval_seconds: 60`
	_, err := configfile.Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for two-schedule job, got nil")
	}
	if !strings.Contains(err.Error(), "two-sched") {
		t.Errorf("error %q does not mention %q", err.Error(), "two-sched")
	}
}

// TestReconcileDisabledNamesPopulated asserts that when prune=true, jobs present
// in the store but absent from the config appear in result.DisabledNames.
func TestReconcileDisabledNamesPopulated(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "tend_prune.db")

	st, err := store.OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}

	clk := clock.NewFake(time.Date(2024, 1, 3, 12, 0, 0, 0, time.UTC))

	jobA := jobs.Job{Name: "job-a", Type: jobs.Shell, Command: "echo a", Cron: "0 1 * * *", Enabled: true}
	jobB := jobs.Job{Name: "job-b", Type: jobs.Shell, Command: "echo b", Cron: "0 2 * * *", Enabled: true}

	// Seed both jobs.
	_, err = configfile.Reconcile(ctx, st, org.ID, clk, nil, configfile.Config{Jobs: []jobs.Job{jobA, jobB}}, true)
	if err != nil {
		t.Fatalf("Reconcile (seed): %v", err)
	}

	// Reconcile with only job-a (prune=true) → job-b should be disabled and reported.
	result, err := configfile.Reconcile(ctx, st, org.ID, clk, nil, configfile.Config{Jobs: []jobs.Job{jobA}}, true)
	if err != nil {
		t.Fatalf("Reconcile (prune): %v", err)
	}
	if result.Disabled != 1 {
		t.Errorf("Disabled = %d; want 1", result.Disabled)
	}
	if len(result.DisabledNames) != 1 || result.DisabledNames[0] != "job-b" {
		t.Errorf("DisabledNames = %v; want [job-b]", result.DisabledNames)
	}

	// Verify job-b is actually disabled in the store.
	storedB, err := st.GetJobByName(ctx, org.ID, "job-b")
	if err != nil {
		t.Fatalf("GetJobByName job-b: %v", err)
	}
	if storedB.Enabled {
		t.Error("job-b should be disabled in the store after prune=true reconcile")
	}

	// job-a should remain enabled and NOT appear in DisabledNames.
	for _, name := range result.DisabledNames {
		if name == "job-a" {
			t.Error("job-a must not appear in DisabledNames")
		}
	}
}

// TestReconcilePruneFalseSkipsDisable asserts that when prune=false, jobs absent
// from the config are left enabled and DisabledNames is empty.
func TestReconcilePruneFalseSkipsDisable(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "tend_noprune.db")

	st, err := store.OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}

	clk := clock.NewFake(time.Date(2024, 1, 3, 12, 0, 0, 0, time.UTC))

	jobA := jobs.Job{Name: "job-a", Type: jobs.Shell, Command: "echo a", Cron: "0 1 * * *", Enabled: true}
	jobB := jobs.Job{Name: "job-b", Type: jobs.Shell, Command: "echo b", Cron: "0 2 * * *", Enabled: true}

	// Seed both jobs.
	_, err = configfile.Reconcile(ctx, st, org.ID, clk, nil, configfile.Config{Jobs: []jobs.Job{jobA, jobB}}, true)
	if err != nil {
		t.Fatalf("Reconcile (seed): %v", err)
	}

	// Reconcile with only job-a and prune=false → job-b must stay enabled, DisabledNames empty.
	result, err := configfile.Reconcile(ctx, st, org.ID, clk, nil, configfile.Config{Jobs: []jobs.Job{jobA}}, false)
	if err != nil {
		t.Fatalf("Reconcile (no-prune): %v", err)
	}
	if result.Disabled != 0 {
		t.Errorf("Disabled = %d; want 0 (prune=false)", result.Disabled)
	}
	if len(result.DisabledNames) != 0 {
		t.Errorf("DisabledNames = %v; want empty (prune=false)", result.DisabledNames)
	}

	// Verify job-b is still enabled in the store.
	storedB, err := st.GetJobByName(ctx, org.ID, "job-b")
	if err != nil {
		t.Fatalf("GetJobByName job-b: %v", err)
	}
	if !storedB.Enabled {
		t.Error("job-b should remain enabled after prune=false reconcile")
	}
}

// TestReconcileManualJobHasZeroNextRunAt asserts that reconciling a no-schedule
// job creates it with a zero NextRunAt (NULL in the DB), so it never auto-fires.
func TestReconcileManualJobHasZeroNextRunAt(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "tend_manual.db")

	st, err := store.OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}

	refTime := time.Date(2024, 1, 3, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(refTime)

	manualJob := jobs.Job{
		Name:    "manual-only",
		Type:    jobs.Shell,
		Command: "echo manual",
		Enabled: true,
		// No Cron, no IntervalSeconds, no RunAt - manual/on-demand only.
	}

	result, err := configfile.Reconcile(ctx, st, org.ID, clk, nil, configfile.Config{Jobs: []jobs.Job{manualJob}}, true)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Created != 1 {
		t.Errorf("Created = %d; want 1", result.Created)
	}

	stored, err := st.GetJobByName(ctx, org.ID, "manual-only")
	if err != nil {
		t.Fatalf("GetJobByName: %v", err)
	}
	if !stored.NextRunAt.IsZero() {
		t.Errorf("NextRunAt = %v; want zero (manual job must not auto-fire)", stored.NextRunAt)
	}
}
