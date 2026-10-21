package jobs

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marsadhq/tend/internal/core"
)

// UnreadableJobError says that a job's row was found but could not be read: a
// column the store decodes (its env, its settings, a timestamp) holds
// something that does not decode, from a hand edit or a bug. It carries what
// was readable, so the job can still be named, failed, repaired or removed.
type UnreadableJobError struct {
	OrgID int64
	JobID int64
	Name  string
	Err   error
}

func (e *UnreadableJobError) Error() string {
	return fmt.Sprintf("job %q (id %d) cannot be read: %v", e.Name, e.JobID, e.Err)
}

func (e *UnreadableJobError) Unwrap() error { return e.Err }

// UnreadableJobsError is what the store's job listings (DueJobs, ListJobs)
// return next to the jobs they could read when some rows could not be: one
// bad row no longer hides every job, nor stops the scheduler.
type UnreadableJobsError struct {
	Jobs []*UnreadableJobError
}

func (e *UnreadableJobsError) Error() string {
	parts := make([]string, len(e.Jobs))
	for i, u := range e.Jobs {
		parts[i] = u.Error()
	}
	return strings.Join(parts, "; ")
}

// UnreadableRunOutput is the output of the failed run recorded for a due job
// whose row cannot be read: why, and what to do about it.
func UnreadableRunOutput(u *UnreadableJobError) string {
	return fmt.Sprintf("FAIL: the job's definition cannot be read, so it was not run and is no longer scheduled: %v\n\n"+
		"Fix it with tend sync (the job's entry overwrites the row), or remove it with tend job rm %s.", u.Err, u.Name)
}

// RunEvent builds a run.* lifecycle event of run for the job named jobName:
// the standard small payload (run_id, job_id, job_name, status, exit_code; no
// secret material, no output), source jobs.runner, and the dedup key
// run:<run_id>:<type>.
func RunEvent(run Run, jobName, typ string, status RunStatus, exitCode int) (core.Event, error) {
	payload, err := json.Marshal(struct {
		RunID    int64  `json:"run_id"`
		JobID    int64  `json:"job_id"`
		JobName  string `json:"job_name"`
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}{
		RunID:    run.ID,
		JobID:    run.JobID,
		JobName:  jobName,
		Status:   string(status),
		ExitCode: exitCode,
	})
	if err != nil {
		return core.Event{}, fmt.Errorf("marshal event payload: %w", err)
	}
	return core.Event{
		OrgID:    run.OrgID,
		Type:     typ,
		Source:   "jobs.runner",
		Payload:  string(payload),
		DedupKey: fmt.Sprintf("run:%d:%s", run.ID, typ),
	}, nil
}
