package hcimport_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the cron checks' time zones

	"github.com/marsadhq/tend/internal/hcimport"
	"github.com/marsadhq/tend/internal/heartbeat"
)

const apiKey = "hcw_secret_api_key_do_not_print"

// managementAPI is a fake Healthchecks Management API serving body for
// GET /api/v3/checks/ to the right X-Api-Key.
func managementAPI(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/checks/" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Api-Key") != apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error": "missing api key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const twoChecks = `{"checks": [
 {"name": "Filesystem Backup", "slug": "filesystem-backup", "tags": "backup fs", "grace": 600, "status": "up",
  "uuid": "5BF66975-D4C7-4BF5-BCC8-B8D8A82EA278", "ping_url": "https://hc-ping.com/5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278", "timeout": 3600},
 {"name": "Database Backup", "slug": "database-backup", "grace": 1200, "status": "down",
  "ping_url": "https://hc-ping.com/0b2f8a8e-4a46-4b0e-9a1c-3f0e2d8f1c55", "schedule": "15 5 * * *", "tz": "UTC"}
]}`

// TestFetch: the checks are read with the key in X-Api-Key, the UUID taken
// from the uuid field or else the ping URL, in lower case.
func TestFetch(t *testing.T) {
	srv := managementAPI(t, twoChecks)
	for _, base := range []string{srv.URL, srv.URL + "/", srv.URL + "/api/v3", srv.URL + "/api/v3/"} {
		checks, err := hcimport.Fetch(context.Background(), nil, base, apiKey)
		if err != nil {
			t.Fatalf("Fetch(%s): %v", base, err)
		}
		if len(checks) != 2 {
			t.Fatalf("Fetch(%s): %d checks, want 2", base, len(checks))
		}
		if checks[0].ID() != "5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278" || checks[1].ID() != "0b2f8a8e-4a46-4b0e-9a1c-3f0e2d8f1c55" {
			t.Fatalf("UUIDs %q, %q", checks[0].ID(), checks[1].ID())
		}
		if checks[0].Timeout != 3600 || checks[0].Grace != 600 || checks[1].Schedule != "15 5 * * *" || checks[1].TZ != "UTC" {
			t.Fatalf("checks = %+v", checks)
		}
	}
}

// TestFetchErrorsNeverCarryTheKey: a refused key, a server error and junk are
// errors that say what went wrong without quoting the key.
func TestFetchErrorsNeverCarryTheKey(t *testing.T) {
	srv := managementAPI(t, twoChecks)
	_, err := hcimport.Fetch(context.Background(), nil, srv.URL, "wrong-"+apiKey)
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), apiKey) {
		t.Fatalf("refused key: err = %v, want a 401 that does not quote the key", err)
	}
	junk := managementAPI(t, `not json`)
	if _, err := hcimport.Fetch(context.Background(), nil, junk.URL, apiKey); err == nil || strings.Contains(err.Error(), apiKey) {
		t.Fatalf("junk body: err = %v", err)
	}
	if _, err := hcimport.Fetch(context.Background(), nil, srv.URL, ""); err == nil {
		t.Fatal("an empty key was sent")
	}
	if _, err := hcimport.Fetch(context.Background(), nil, "ftp://nowhere", apiKey); err == nil {
		t.Fatal("a non-http URL was accepted")
	}
}

// TestReadOnlyKeyIsRefused: a read-only key gets no UUIDs, which are what
// keeps the ping URLs working, so the import says to use the read-write one.
func TestReadOnlyKeyIsRefused(t *testing.T) {
	srv := managementAPI(t, `{"checks": [{"name": "x", "slug": "x", "unique_key": "a6c7b0a8a66bed0df66abfdab3c77736861703ee", "timeout": 60, "grace": 60}]}`)
	_, err := hcimport.Fetch(context.Background(), nil, srv.URL, apiKey)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("read-only key: err = %v, want a read-only error", err)
	}
}

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func check(name, slug, uuid string, timeout, grace int) hcimport.Check {
	return hcimport.Check{Name: name, Slug: slug, UUID: uuid, Timeout: timeout, Grace: grace, Status: "up"}
}

// TestPlanSimpleChecks: a simple check becomes a heartbeat named after its
// slug (else its name, else its UUID) with the UUID as its token, timeout as
// period and its grace; one that is already imported is updated or left alone.
func TestPlanSimpleChecks(t *testing.T) {
	checks := []hcimport.Check{
		check("Filesystem Backup", "filesystem-backup", "5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278", 3600, 600),
		check("Plain Name", "", "11111111-2222-4333-8444-555555555555", 60, 60),
		check("", "", "66666666-7777-4888-9999-000000000000", 86400, 3600),
		check("Again", "filesystem-backup", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", 120, 60),
		check("Imported", "imported", "12345678-1234-4234-8234-123456789012", 300, 60),
		check("Same", "same", "abcdefab-cdef-4abc-8def-abcdefabcdef", 300, 60),
		check("Taken", "taken", "fedcbafe-dcba-4fed-8cba-fedcbafedcba", 300, 60),
	}
	existing := []heartbeat.Heartbeat{
		{Name: "renamed-in-tend", Token: "12345678-1234-4234-8234-123456789012", PeriodSeconds: 600, GraceSeconds: 60},
		{Name: "same", Token: "abcdefab-cdef-4abc-8def-abcdefabcdef", PeriodSeconds: 300, GraceSeconds: 60},
		{Name: "taken", Token: "0123456789abcdef0123456789abcdef", PeriodSeconds: 60},
	}
	plan := hcimport.Plan(checks, existing, hcimport.Options{Now: now})
	want := []struct {
		op, name      string
		period, grace int
	}{
		{hcimport.OpCreate, "filesystem-backup", 3600, 600},
		{hcimport.OpCreate, "Plain Name", 60, 60},
		{hcimport.OpCreate, "66666666-7777-4888-9999-000000000000", 86400, 3600},
		{hcimport.OpCreate, "filesystem-backup-aaaaaaaa", 120, 60},
		{hcimport.OpUpdate, "renamed-in-tend", 300, 60},
		{hcimport.OpUnchanged, "same", 300, 60},
		{hcimport.OpSkip, "taken", 300, 60},
	}
	if len(plan) != len(want) {
		t.Fatalf("plan has %d actions, want %d", len(plan), len(want))
	}
	for i, w := range want {
		a := plan[i]
		if a.Op != w.op || a.Name != w.name || a.PeriodSeconds != w.period || a.GraceSeconds != w.grace || a.Token != checks[i].UUID {
			t.Errorf("action %d = %+v, want %s %s %d/%d", i, a, w.op, w.name, w.period, w.grace)
		}
	}
	if len(plan[3].Warnings) != 1 || !strings.Contains(plan[3].Warnings[0], "filesystem-backup") {
		t.Errorf("the renamed duplicate slug has warnings %q, want one about the name", plan[3].Warnings)
	}
	if !strings.Contains(plan[6].Reason, "already exists") {
		t.Errorf("conflict reason = %q", plan[6].Reason)
	}
}

// TestPlanCronChecks: Tend heartbeats have no schedule, so a cron check gets
// a period approximating it, the longest gap between two of its runs, and a
// warning that says so; one whose schedule cannot be read gets a day.
func TestPlanCronChecks(t *testing.T) {
	cron := func(slug, schedule, tz string) hcimport.Check {
		c := check(slug, slug, "", 0, 600)
		c.UUID, c.Schedule, c.TZ = "5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278", schedule, tz
		return c
	}
	for _, c := range []struct {
		check    hcimport.Check
		period   time.Duration
		warnings []string
	}{
		{cron("daily", "15 5 * * *", "UTC"), 24 * time.Hour, []string{`"daily" is a cron check ("15 5 * * *", UTC)`, "period of 24h0m0s", "the time between its runs"}},
		{cron("weekdays", "0 9 * * 1-5", "UTC"), 72 * time.Hour, []string{"period of 72h0m0s", "longest gap", "shortest is 24h0m0s", "noticed later"}},
		{cron("riga", "0 12 * * *", "Europe/Riga"), 25 * time.Hour, []string{"Europe/Riga", "period of 25h0m0s"}},
		{cron("hourly", "@hourly", ""), time.Hour, []string{"period of 1h0m0s"}},
		{cron("calendar", "Mon *-*-* 09:00", "UTC"), 24 * time.Hour, []string{"could not read", "tend heartbeat add -name calendar -period"}},
		{cron("badzone", "0 * * * *", "Mars/Olympus"), time.Hour, []string{`unknown time zone "Mars/Olympus"`}},
	} {
		plan := hcimport.Plan([]hcimport.Check{c.check}, nil, hcimport.Options{Now: now})
		a := plan[0]
		if a.Op != hcimport.OpCreate || time.Duration(a.PeriodSeconds)*time.Second != c.period || a.GraceSeconds != 600 {
			t.Errorf("%s: %+v, want create with period %v and grace 600", c.check.Slug, a, c.period)
			continue
		}
		all := strings.Join(a.Warnings, "\n")
		for _, w := range c.warnings {
			if !strings.Contains(all, w) {
				t.Errorf("%s: warnings %q, want %q", c.check.Slug, all, w)
			}
		}
	}
}

// TestPlanPausedChecks: a paused check is left out unless asked for (Tend
// has no pause, so it would alert as soon as its period passed).
func TestPlanPausedChecks(t *testing.T) {
	paused := check("Old", "old", "5bf66975-d4c7-4bf5-bcc8-b8d8a82ea278", 60, 60)
	paused.Status = "paused"
	if a := hcimport.Plan([]hcimport.Check{paused}, nil, hcimport.Options{Now: now})[0]; a.Op != hcimport.OpSkip || !strings.Contains(a.Reason, "paused") {
		t.Fatalf("paused check: %+v, want skipped", a)
	}
	a := hcimport.Plan([]hcimport.Check{paused}, nil, hcimport.Options{Now: now, IncludePaused: true})[0]
	if a.Op != hcimport.OpCreate || len(a.Warnings) == 0 || !strings.Contains(a.Warnings[0], "paused") {
		t.Fatalf("paused check with IncludePaused: %+v, want created with a warning", a)
	}
}
