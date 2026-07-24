package jobs

import (
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ValidateCron returns an error if expr is not a schedule the runner can
// parse. It uses the same parser as NextRun, so an expression that validates
// here is guaranteed to schedule (and vice-versa) - validation and execution
// can never diverge.
func ValidateCron(expr string) error {
	if _, err := cronParser.Parse(expr); err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return nil
}

// NextRun returns the next time this job should fire strictly after `now`.
// A zero time means "never again" (e.g. an elapsed one-off).
func (j Job) NextRun(now time.Time) (time.Time, error) {
	switch {
	case j.Cron != "":
		sched, err := cronParser.Parse(j.Cron)
		if err != nil {
			return time.Time{}, err
		}
		return sched.Next(now), nil
	case j.IntervalSeconds > 0:
		return now.Add(time.Duration(j.IntervalSeconds) * time.Second), nil
	case !j.RunAt.IsZero():
		if j.RunAt.After(now) {
			return j.RunAt, nil
		}
		return time.Time{}, nil // already passed
	default:
		return time.Time{}, errors.New("job has no schedule")
	}
}
