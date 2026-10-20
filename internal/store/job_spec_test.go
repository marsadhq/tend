package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// TestJobSpecRoundTrip stores a job of each type that has settings and reads
// it back through every way a job is read, on both backends.
func TestJobSpecRoundTrip(t *testing.T) {
	expires := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	cases := []jobs.Job{
		{Name: "web-cert", Type: jobs.TLS, Spec: &jobs.Spec{
			Host: "example.com", Port: 587, ServerName: "mail.example.com", StartTLS: "smtp",
			InsecureSkipVerify: true, CAFile: "/etc/ssl/private-ca.pem", WarnDays: 14, RenewAfterFraction: 2.0 / 3,
		}},
		{Name: "domain", Type: jobs.Domain, Spec: &jobs.Spec{Domain: "example.com", RDAPURL: "https://rdap.example/", WarnDays: 30}},
		{Name: "licence", Type: jobs.Expiry, Spec: &jobs.Spec{ExpiresAt: expires, WarnDays: 0}},
		{Name: "node-key", Type: jobs.Expiry, Spec: &jobs.Spec{ExpiryCommand: "tailscale status --json | jq -r .Self.KeyExpiry", WarnDays: 7}},
		{Name: "drill", Type: jobs.Drill, Spec: &jobs.Spec{
			Restore: `cp /backup/app.db "$TEND_SCRATCH/app.db"`,
			Checks:  []string{`sqlite3 "$TEND_SCRATCH/app.db" 'PRAGMA integrity_check'`, "true"},
			Cleanup: "true", ScratchDir: "/var/tmp", MaxDurationSeconds: 600,
		}},
	}
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		due := time.Now().Add(-time.Minute)
		for _, c := range cases {
			c.OrgID, c.Enabled, c.NextRunAt = org.ID, true, due
			id, err := s.CreateJob(ctx, c)
			if err != nil {
				t.Fatalf("CreateJob %s: %v", c.Name, err)
			}
			got, err := s.GetJob(ctx, org.ID, id)
			if err != nil {
				t.Fatalf("GetJob %s: %v", c.Name, err)
			}
			assertSpec(t, "GetJob "+c.Name, got.Spec, c.Spec)
			byName, err := s.GetJobByName(ctx, org.ID, c.Name)
			if err != nil {
				t.Fatalf("GetJobByName %s: %v", c.Name, err)
			}
			assertSpec(t, "GetJobByName "+c.Name, byName.Spec, c.Spec)
		}

		list, err := s.ListJobs(ctx, org.ID)
		if err != nil || len(list) != len(cases) {
			t.Fatalf("ListJobs: %v, %d jobs", err, len(list))
		}
		for i, j := range list {
			assertSpec(t, "ListJobs "+j.Name, j.Spec, cases[i].Spec)
		}
		dueJobs, err := s.DueJobs(ctx, time.Now())
		if err != nil || len(dueJobs) != len(cases) {
			t.Fatalf("DueJobs: %v, %d jobs", err, len(dueJobs))
		}
		for i, j := range dueJobs {
			assertSpec(t, "DueJobs "+j.Name, j.Spec, cases[i].Spec)
		}
	})
}

// TestJobSpecUpdate changes a job's settings, then turns it into a shell job,
// which has none.
func TestJobSpecUpdate(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "cert", Type: jobs.TLS, Enabled: true,
			Spec: &jobs.Spec{Host: "a.example", WarnDays: 14, RenewAfterFraction: 2.0 / 3}})
		if err != nil {
			t.Fatal(err)
		}
		j, err := s.GetJob(ctx, org.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		j.Spec = &jobs.Spec{Host: "b.example", Port: 8443, WarnDays: 0}
		if err := s.UpdateJob(ctx, j); err != nil {
			t.Fatalf("UpdateJob: %v", err)
		}
		got, err := s.GetJob(ctx, org.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		assertSpec(t, "after update", got.Spec, j.Spec)

		got.Type, got.Command, got.Spec = jobs.Shell, "echo hi", nil
		if err := s.UpdateJob(ctx, got); err != nil {
			t.Fatalf("UpdateJob to shell: %v", err)
		}
		shell, err := s.GetJob(ctx, org.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if shell.Spec != nil {
			t.Fatalf("shell job kept a spec: %+v", shell.Spec)
		}
	})
}

// TestJobWithoutSpecStoresNull checks that shell and http jobs, the only jobs
// there were before the spec column, still read back without settings and
// leave the column NULL.
func TestJobWithoutSpecStoresNull(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.store
			org, err := s.BootstrapDefaultOrg(ctx)
			if err != nil {
				t.Fatal(err)
			}
			id, err := s.CreateJob(ctx, jobs.Job{OrgID: org.ID, Name: "hello", Type: jobs.Shell, Command: "echo hi", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.GetJob(ctx, org.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Spec != nil {
				t.Fatalf("shell job read back with a spec: %+v", got.Spec)
			}
			var isNull bool
			if err := store.RawDB(s).QueryRowContext(ctx,
				`SELECT spec IS NULL FROM jobs WHERE id = `+ph(b.name, 1), id).Scan(&isNull); err != nil {
				t.Fatalf("read spec column: %v", err)
			}
			if !isNull {
				t.Fatal("spec column is not NULL for a shell job")
			}
		})
	}
}

func assertSpec(t *testing.T, what string, got, want *jobs.Spec) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s: spec = %+v, want %+v", what, got, want)
	}
	if got == nil {
		return
	}
	g, w := *got, *want
	if !g.ExpiresAt.Equal(w.ExpiresAt) {
		t.Fatalf("%s: expires_at = %v, want %v", what, g.ExpiresAt, w.ExpiresAt)
	}
	g.ExpiresAt, w.ExpiresAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s: spec = %+v, want %+v", what, g, w)
	}
}
