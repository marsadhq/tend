package jobs

import (
	"encoding/json"
	"time"

	"github.com/marsadhq/tend/internal/core"
)

// FailureDetail is the Detail of a run.failed: what its alert says beyond the
// payload's status and exit code. It is for the notification text only; the
// webhook's event and the marsad envelope never carry it.
type FailureDetail struct {
	// Output is the tail of the output the run stored (core.OutputTail), so
	// already cleaned and with the job's secret values redacted.
	Output string `json:"output,omitempty"`
	// TimeoutSeconds is the per-attempt timeout a timed-out run reached.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// failureDetail is the Detail of the run.failed of a run of job that ended in
// status with the stored output out, or "" when there is nothing to say.
func failureDetail(job Job, status RunStatus, out string) string {
	d := FailureDetail{Output: core.OutputTail(out)}
	if status == StatusTimedOut {
		timeout := seconds(job.TimeoutSeconds)
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		d.TimeoutSeconds = int(timeout / time.Second)
	}
	if d == (FailureDetail{}) {
		return ""
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "" // cannot happen for this struct; the alert then says less
	}
	return string(b)
}
