package httpserver_test

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/tend/internal/httpserver"
	"github.com/marsadhq/tend/internal/jobs"
)

// seedSpecJob creates a job of type typ with settings spec under orgID.
func seedSpecJob(t *testing.T, ts *testStore, orgID int64, name string, typ jobs.JobType, spec *jobs.Spec) int64 {
	t.Helper()
	id, err := ts.store.CreateJob(context.Background(), jobs.Job{
		OrgID: orgID, Name: name, Type: typ, IntervalSeconds: 3600, Enabled: true, Spec: spec,
	})
	if err != nil {
		t.Fatalf("CreateJob(%q): %v", name, err)
	}
	return id
}

func TestAPIJobSpec(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	tlsID := seedSpecJob(t, ts, ts.orgID, "mail-cert", jobs.TLS,
		&jobs.Spec{Host: "mail.example.com", Port: 587, StartTLS: "smtp", WarnDays: 0, RenewAfterFraction: 2.0 / 3})
	drillID := seedSpecJob(t, ts, ts.orgID, "drill", jobs.Drill,
		&jobs.Spec{Restore: "restore-it", Checks: []string{"check-a", "check-b"}, MaxDurationSeconds: 600})
	expiryID := seedSpecJob(t, ts, ts.orgID, "licence", jobs.Expiry,
		&jobs.Spec{ExpiresAt: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), WarnDays: 14})
	shellID := seedJob(t, ts, ts.orgID, "plain")
	h := newAuthServer(t, ts.store, authConfig(false)).Handler()

	get := func(id int64) map[string]any {
		t.Helper()
		rec := apiGet(t, h, as, "/api/jobs/"+itoa(id))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d; body=%q", rec.Code, rec.Body.String())
		}
		var m map[string]any
		decodeJSON(t, rec, &m)
		return m
	}

	spec, ok := get(tlsID)["spec"].(map[string]any)
	if !ok {
		t.Fatalf("tls job has no spec object")
	}
	for k, want := range map[string]any{"host": "mail.example.com", "port": 587.0, "starttls": "smtp", "warn_days": 0.0} {
		if spec[k] != want {
			t.Errorf("tls spec[%q] = %v, want %v (spec %v)", k, spec[k], want, spec)
		}
	}
	if f, _ := spec["renew_after_fraction"].(float64); f < 0.66 || f > 0.67 {
		t.Errorf("tls spec renew_after_fraction = %v", spec["renew_after_fraction"])
	}

	dspec, _ := get(drillID)["spec"].(map[string]any)
	checks, _ := dspec["checks"].([]any)
	if dspec["restore"] != "restore-it" || len(checks) != 2 || dspec["max_duration_seconds"] != 600.0 {
		t.Errorf("drill spec = %v", dspec)
	}
	if _, has := dspec["warn_days"]; has {
		t.Errorf("drill spec carries warn_days: %v", dspec)
	}

	espec, _ := get(expiryID)["spec"].(map[string]any)
	if espec["expires_at"] != "2027-01-31T00:00:00Z" || espec["warn_days"] != 14.0 {
		t.Errorf("expiry spec = %v", espec)
	}

	if _, has := get(shellID)["spec"]; has {
		t.Error("a shell job's DTO has a spec")
	}
	// The list carries it too.
	rec := apiGet(t, h, as, "/api/jobs")
	var list []map[string]json.RawMessage
	decodeJSON(t, rec, &list)
	withSpec := 0
	for _, j := range list {
		if _, has := j["spec"]; has {
			withSpec++
		}
	}
	if withSpec != 3 {
		t.Errorf("%d listed jobs carry a spec, want 3", withSpec)
	}
}

func TestDashboardJobDetailShowsProbeTargets(t *testing.T) {
	ts := newStore(t)
	as := seedAuth(t, ts)
	cases := map[int64]string{
		seedSpecJob(t, ts, ts.orgID, "web-cert", jobs.TLS, &jobs.Spec{Host: "example.com", WarnDays: 14}):                         "example.com:443",
		seedSpecJob(t, ts, ts.orgID, "mail-cert", jobs.TLS, &jobs.Spec{Host: "mail.example.com", StartTLS: "smtp", WarnDays: 14}): "mail.example.com:25 (STARTTLS smtp)",
		seedSpecJob(t, ts, ts.orgID, "domain", jobs.Domain, &jobs.Spec{Domain: "example.com", WarnDays: 30}):                      "example.com (warns 30 days before)",
		seedSpecJob(t, ts, ts.orgID, "licence", jobs.Expiry,
			&jobs.Spec{ExpiresAt: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), WarnDays: 14}): "2027-01-31T00:00:00Z (warns 14 days before)",
		seedSpecJob(t, ts, ts.orgID, "node-key", jobs.Expiry, &jobs.Spec{ExpiryCommand: "tailscale-expiry", WarnDays: 3}):           "date from: tailscale-expiry",
		seedSpecJob(t, ts, ts.orgID, "drill", jobs.Drill, &jobs.Spec{Restore: "restic restore latest", Checks: []string{"a", "b"}}): "restic restore latest (2 checks)",
	}
	codec := testCodec()
	h := newAuthServer(t, ts.store, &httpserver.AuthConfig{Codec: codec}).Handler()
	cookie, _ := validSessionCookie(t, as, codec)
	for id, want := range cases {
		rec := dashGet(t, h, cookie, "/jobs/"+itoa(id))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d; body=%q", rec.Code, rec.Body.String())
		}
		if body := html.UnescapeString(rec.Body.String()); !strings.Contains(body, want) {
			t.Errorf("job %d detail lacks target %q", id, want)
		}
	}
}
