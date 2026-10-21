package configfile_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the raw handle that corrupts a row

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/tend/internal/configfile"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// TestReconcileRepairsUnreadableJob: tend sync is how a job whose row cannot
// be read is fixed. A job in the file overwrites the unreadable row in place
// and is scheduled again; an unreadable job absent from the file does not
// stop the sync.
func TestReconcileRepairsUnreadableJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tend.db")
	st, err := store.OpenSQLite(path)
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
	cfg := configfile.Config{Jobs: []jobs.Job{
		{Name: "nightly", Type: jobs.Shell, Command: "true", Cron: "0 3 * * *", Enabled: true, Env: map[string]string{"A": "b"}},
		{Name: "orphan", Type: jobs.Shell, Command: "true", Cron: "0 4 * * *", Enabled: true},
	}}
	if _, err := configfile.Reconcile(ctx, st, org.ID, clk, nil, cfg, true); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `UPDATE jobs SET env = '{not json', next_run = NULL WHERE name IN ('nightly', 'orphan')`); err != nil {
		t.Fatal(err)
	}

	res, err := configfile.Reconcile(ctx, st, org.ID, clk, nil, configfile.Config{Jobs: cfg.Jobs[:1]}, true)
	if err != nil {
		t.Fatalf("Reconcile over an unreadable job: %v", err)
	}
	if res.Updated != 1 || res.Created != 0 {
		t.Fatalf("result %+v, want the job updated in place", res)
	}
	j, err := st.GetJobByName(ctx, org.ID, "nightly")
	if err != nil {
		t.Fatalf("the job is still unreadable: %v", err)
	}
	if j.Env["A"] != "b" || j.NextRunAt.IsZero() {
		t.Fatalf("repaired job = %+v", j)
	}
}
