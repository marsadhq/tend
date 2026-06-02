// Package jobs defines the core domain types for scheduled units of work and
// their executions. Logic (scheduling, execution, the runner loop) is layered
// on top of these types by later tasks; this file holds only the data shapes.
package jobs

import "time"

// JobType identifies the kind of work a Job performs.
type JobType string

const (
	// Shell runs a command via the system shell.
	Shell JobType = "shell"
	// HTTP performs an HTTP request.
	HTTP JobType = "http"
)

// Job is a scheduled unit of work. Tenant-scoped via OrgID.
type Job struct {
	ID              int64
	OrgID           int64
	Name            string
	Type            JobType
	Command         string    // shell jobs
	HTTPURL         string    // http jobs
	HTTPMethod      string    // http jobs (default GET when empty)
	HTTPBody        string    // http jobs
	Cron            string    // cron expression (mutually exclusive with the others)
	IntervalSeconds int       // fixed interval schedule
	RunAt           time.Time // one-off schedule (zero = unset)
	TimeoutSeconds  int       // 0 = executor default
	MaxRetries      int       // additional attempts after the first
	Enabled         bool
	Env             map[string]string // per-job env vars; values may be secret refs (resolved later)
	NextRunAt       time.Time         // persisted next fire time (zero = unscheduled)
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// RunStatus is the lifecycle state of a single Run.
type RunStatus string

const (
	StatusPending   RunStatus = "pending"
	StatusRunning   RunStatus = "running"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
	StatusTimedOut  RunStatus = "timed_out"
)

// Run is one execution attempt of a Job (a job_runs row).
type Run struct {
	ID        int64
	OrgID     int64
	JobID     int64
	Status    RunStatus
	Attempt   int
	ExitCode  int
	Output    string
	ClaimedBy string
	StartedAt time.Time
	EndedAt   time.Time
	CreatedAt time.Time
}
