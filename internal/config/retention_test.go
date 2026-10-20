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
	r, err := LoadRetention()
	if err != nil {
		t.Fatal(err)
	}
	if r.Events != 30*day || r.JobRuns != 90*day || r.Deliveries != 7*day {
		t.Fatalf("defaults = %+v, want 30d/90d/7d", r)
	}
	if got := r.String(); got != "events 30d, job runs 90d, deliveries 7d" {
		t.Errorf("String() = %q", got)
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
	if got := r.String(); !strings.Contains(got, "job runs kept forever") {
		t.Errorf("String() = %q, want job runs kept forever", got)
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
