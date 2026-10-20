package config

import (
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

func TestLoadRetentionDefaults(t *testing.T) {
	t.Setenv(EnvEventRetentionDays, "")
	t.Setenv(EnvJobRunRetentionDays, "")
	t.Setenv(EnvDeliveryRetentionDays, "")
	t.Setenv(EnvPingRetentionDays, "")
	r, err := LoadRetention()
	if err != nil {
		t.Fatal(err)
	}
	if r.Events != 30*day || r.JobRuns != 90*day || r.Deliveries != 7*day || r.Pings != 30*day {
		t.Fatalf("defaults = %+v, want 30d/90d/7d/30d", r)
	}
	// The job-run default went from 30 to 90 days in v0.3.0; the banner says so.
	if got, want := r.String(), "events 30d, job runs 90d (default, up from 30d before v0.3.0; set TEND_RETENTION_RUNS_DAYS to change), deliveries 7d, heartbeat pings 30d"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestLoadRetentionOverridesAndZero(t *testing.T) {
	t.Setenv(EnvEventRetentionDays, "365")
	t.Setenv(EnvJobRunRetentionDays, "0")
	t.Setenv(EnvDeliveryRetentionDays, " 1 ")
	r, err := LoadRetention()
	if err != nil {
		t.Fatal(err)
	}
	if r.Events != 365*day || r.JobRuns != 0 || r.Deliveries != day {
		t.Fatalf("got %+v, want 365d/0/1d", r)
	}
	if got := r.String(); !strings.Contains(got, "job runs kept forever,") {
		t.Errorf("String() = %q, want job runs kept forever (and no default note: the variable is set)", got)
	}
}

func TestLoadRetentionRejectsBadValues(t *testing.T) {
	for _, v := range []string{"-1", "30d", "1.5", "abc", "36501"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(EnvEventRetentionDays, "")
			t.Setenv(EnvJobRunRetentionDays, v)
			t.Setenv(EnvDeliveryRetentionDays, "")
			_, err := LoadRetention()
			if err == nil || !strings.Contains(err.Error(), EnvJobRunRetentionDays) {
				t.Fatalf("LoadRetention with %s=%q: err = %v, want an error naming the variable", EnvJobRunRetentionDays, v, err)
			}
		})
	}
}

func TestLoadRetentionPings(t *testing.T) {
	t.Setenv(EnvPingRetentionDays, "3")
	r, err := LoadRetention()
	if err != nil {
		t.Fatal(err)
	}
	if r.Pings != 3*day {
		t.Fatalf("Pings = %v, want 3 days", r.Pings)
	}
	t.Setenv(EnvPingRetentionDays, "0")
	if r, err = LoadRetention(); err != nil || r.Pings != 0 || !strings.Contains(r.String(), "heartbeat pings kept forever") {
		t.Fatalf("0 days: %+v, %v; want kept forever", r, err)
	}
	t.Setenv(EnvPingRetentionDays, "x")
	if _, err := LoadRetention(); err == nil || !strings.Contains(err.Error(), EnvPingRetentionDays) {
		t.Fatalf("bad value: err = %v, want an error naming %s", err, EnvPingRetentionDays)
	}
}
