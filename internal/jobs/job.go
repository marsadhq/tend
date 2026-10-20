// Package jobs defines the core domain types for scheduled units of work and
// their executions. Logic (scheduling, execution, the runner loop) is layered
// on top of these types by later tasks; this file holds only the data shapes.
package jobs

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/marsadhq/tend/internal/core"
)

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
	TimeoutSeconds  int       // <= 0 = executor default (DefaultTimeout); input refuses negatives
	MaxRetries      int       // additional attempts after the first
	Enabled         bool
	Env             map[string]string // per-job env vars; values may be secret refs (resolved later)
	Spec            *Spec             // tls, domain, expiry and drill settings; nil for shell and http jobs
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

// Event types of a run's lifecycle. Every run.* event carries the same small
// JSON payload (run_id, job_id, job_name, status, exit_code) and the dedup key
// run:<run_id>:<type>.
const (
	EventRunStarted   = "run.started"
	EventRunSucceeded = "run.succeeded"
	EventRunFailed    = "run.failed"
	// EventRunRecovered is recorded next to run.succeeded when the job's
	// previous finished run had not succeeded (failed, timed out or reaped).
	EventRunRecovered = "run.recovered"
)

// RecoveredEvent derives the run.recovered event of run runID from its
// run.succeeded event: the same org, source and payload, its own type and
// dedup key. The store records it in the transaction that finishes the run,
// where it can see the job's previous outcome.
func RecoveredEvent(succeeded core.Event, runID int64) core.Event {
	ev := succeeded
	ev.ID = 0
	ev.Type = EventRunRecovered
	ev.DedupKey = fmt.Sprintf("run:%d:%s", runID, EventRunRecovered)
	return ev
}

// EventJobRemoved is recorded when a job is deleted. Its payload is
// {"job_id":<id>,"job_name":"<name>"}, so rules scoped to the job match it, and
// its dedup key is job:<id>:removed.
const EventJobRemoved = "job.removed"

// RemovedEvent is the job.removed event for job id, named name, of org orgID.
func RemovedEvent(orgID, id int64, name string) core.Event {
	payload, _ := json.Marshal(struct {
		JobID   int64  `json:"job_id"`
		JobName string `json:"job_name"`
	}{id, name}) // cannot fail: an int and a string
	return core.Event{
		OrgID:    orgID,
		Type:     EventJobRemoved,
		Source:   "jobs",
		Payload:  string(payload),
		DedupKey: fmt.Sprintf("job:%d:removed", id),
	}
}
