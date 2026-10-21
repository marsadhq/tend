package jobs

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/marsadhq/tend/internal/core"
)

// FailureDetail is the Detail of a run.failed: what its alert says beyond the
// payload's status and exit code. It is for the notification text only; the
// webhook's event and the marsad envelope never carry it.
type FailureDetail struct {
	// Output is the tail of the output the run stored, so already cleaned and
	// with the job's secret values redacted: its last core.MaxOutputTailLines
	// lines, at most core.OutputTailBytes, of which an alert shows as many
	// lines as it is set to.
	Output string `json:"output,omitempty"`
	// Truncated is set when the run's output was cut at 1 MiB: Output is
	// then the tail of what came before the cut, not the end of what the run
	// printed, and the line the cut fell in is left out of it.
	Truncated bool `json:"truncated,omitempty"`
	// TimeoutSeconds is the per-attempt timeout a timed-out run reached.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// failureDetail is the Detail of the run.failed of a run of job that ended in
// status with the stored output out, or "" when there is nothing to say.
func failureDetail(job Job, status RunStatus, out string) string {
	var d FailureDetail
	if body, cut := strings.CutSuffix(out, truncationMarker); cut {
		// The cut can fall inside anything, part of a secret printed in a
		// form redaction does not recognise included: its line stays out.
		out, d.Truncated = dropLastLine(body), true
	}
	d.Output = core.TailLines(out, core.MaxOutputTailLines)
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

// dropLastLine returns s without its last line ("" for a single line).
func dropLastLine(s string) string {
	i := strings.LastIndexByte(s, '\n')
	if i < 0 {
		return ""
	}
	return s[:i]
}
