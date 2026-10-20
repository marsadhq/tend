package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Retention is how long serve's daily sweep keeps each kind of history. A zero
// window keeps that history forever (the prune is skipped).
type Retention struct {
	Events     time.Duration // events, except those a pending delivery still needs
	JobRuns    time.Duration // finished job runs; pending and running runs are never pruned
	Deliveries time.Duration // delivered and failed deliveries; pending ones are never pruned
}

// Retention defaults, in days. Events are the alert record (a month); job runs
// are the run history behind the dashboard (a quarter); finalized deliveries
// only matter while debugging a recent notification problem (a week).
const (
	DefaultEventRetentionDays    = 30
	DefaultJobRunRetentionDays   = 90
	DefaultDeliveryRetentionDays = 7
)

// maxRetentionDays bounds a window so the cutoff arithmetic cannot overflow.
const maxRetentionDays = 100 * 365

// Retention window environment variables. Each takes a whole number of days;
// 0 turns that prune off.
const (
	EnvEventRetentionDays    = "TEND_RETENTION_EVENTS_DAYS"
	EnvJobRunRetentionDays   = "TEND_RETENTION_RUNS_DAYS"
	EnvDeliveryRetentionDays = "TEND_RETENTION_DELIVERIES_DAYS"
)

// LoadRetention reads the retention windows from the environment, falling back
// to the defaults for unset variables. A value that is not a whole number of
// days between 0 and 36500 is an error naming the variable: a typo must not
// silently keep (or drop) a year of history.
func LoadRetention() (Retention, error) {
	var r Retention
	for _, w := range []struct {
		env  string
		def  int
		dest *time.Duration
	}{
		{EnvEventRetentionDays, DefaultEventRetentionDays, &r.Events},
		{EnvJobRunRetentionDays, DefaultJobRunRetentionDays, &r.JobRuns},
		{EnvDeliveryRetentionDays, DefaultDeliveryRetentionDays, &r.Deliveries},
	} {
		days := w.def
		if v := strings.TrimSpace(os.Getenv(w.env)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > maxRetentionDays {
				return Retention{}, fmt.Errorf("%s=%q: want a whole number of days from 0 (keep forever) to %d", w.env, v, maxRetentionDays)
			}
			days = n
		}
		*w.dest = time.Duration(days) * 24 * time.Hour
	}
	return r, nil
}

// String renders the windows for the serve banner, e.g.
// "events 30d, job runs 90d, deliveries 7d".
func (r Retention) String() string {
	f := func(d time.Duration) string {
		if d == 0 {
			return "kept forever"
		}
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return fmt.Sprintf("events %s, job runs %s, deliveries %s", f(r.Events), f(r.JobRuns), f(r.Deliveries))
}
