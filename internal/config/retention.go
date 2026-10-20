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
	Pings      time.Duration // heartbeat ping log entries (each heartbeat also keeps only its newest 1000)

	// jobRunsByDefault records that JobRuns is the default because
	// TEND_RETENTION_RUNS_DAYS is unset, so String can point out that the
	// default changed.
	jobRunsByDefault bool
}

// Retention defaults, in days. Events are the alert record (a month); job runs
// are the run history behind the dashboard (a quarter; v0.3.0 raised it from
// the month earlier versions kept); finalized deliveries only matter while
// debugging a recent notification problem (a week); a heartbeat's ping log is
// kept as long as the events it explains (a month).
const (
	DefaultEventRetentionDays    = 30
	DefaultJobRunRetentionDays   = 90
	DefaultDeliveryRetentionDays = 7
	DefaultPingRetentionDays     = 30
)

// maxRetentionDays bounds a window so the cutoff arithmetic cannot overflow.
const maxRetentionDays = 100 * 365

// Retention window environment variables. Each takes a whole number of days;
// 0 turns that prune off.
const (
	EnvEventRetentionDays    = "TEND_RETENTION_EVENTS_DAYS"
	EnvJobRunRetentionDays   = "TEND_RETENTION_RUNS_DAYS"
	EnvDeliveryRetentionDays = "TEND_RETENTION_DELIVERIES_DAYS"
	EnvPingRetentionDays     = "TEND_RETENTION_PINGS_DAYS"
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
		{EnvPingRetentionDays, DefaultPingRetentionDays, &r.Pings},
	} {
		days := w.def
		if w.env == EnvJobRunRetentionDays && strings.TrimSpace(os.Getenv(w.env)) == "" {
			r.jobRunsByDefault = true
		}
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

// String renders the windows for the serve banner and doctor, e.g.
// "events 30d, job runs 90d (default, up from 30d before v0.3.0), deliveries
// 7d, heartbeat pings 30d". The note about the job-run default stays while that default is in
// effect, so an upgrade that suddenly keeps three times as much run history
// says so where the operator looks.
func (r Retention) String() string {
	f := func(d time.Duration) string {
		if d == 0 {
			return "kept forever"
		}
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	runs := f(r.JobRuns)
	if r.jobRunsByDefault {
		runs += fmt.Sprintf(" (default, up from 30d before v0.3.0; set %s to change)", EnvJobRunRetentionDays)
	}
	return fmt.Sprintf("events %s, job runs %s, deliveries %s, heartbeat pings %s", f(r.Events), runs, f(r.Deliveries), f(r.Pings))
}
