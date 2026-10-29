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
	// Verdict is the paragraph a probe's or a drill's output opens with: a
	// FAIL: line per problem, or why it could not check. It is kept apart,
	// from its start (at most core.MaxOutputTailLines lines and
	// core.OutputTailBytes), because the tail of such an output shows only
	// what was checked, cleaned up and printed; Output is then the tail of
	// what follows it. Shell and http jobs have none.
	Verdict string `json:"verdict,omitempty"`
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
	// HTTPStatus is the status of the response an http job got, which is
	// also its exit code. Its response body is not in Output: a server can
	// echo the request, a token in the URL's path or query included. The
	// error of a request that got no response is, naming only the host.
	HTTPStatus int `json:"http_status,omitempty"`
	// Streak is the job's failing streak this run is part of. The store
	// records it, in the transaction that records the event (WithStreak).
	Streak
}

// Streak is a job's failing streak: how many runs in a row, the one an
// event is about included, have not succeeded (failed, timed out or reaped),
// and when the first of them was recorded. The store keeps it on the job,
// adds it to each run.failed's detail (WithStreak) and gives the run.recovered
// that ends it the streak it ended (RecoveryDetail); a success starts over.
type Streak struct {
	FailedRuns   int       `json:"failed_runs,omitempty"`
	FailingSince time.Time `json:"failing_since,omitzero"`
}

// WithStreak is the detail of a run.failed with s added after what detail
// says. A detail that is not a FailureDetail is returned as it is.
func WithStreak(detail string, s Streak) string {
	var d FailureDetail
	if detail != "" && json.Unmarshal([]byte(detail), &d) != nil {
		return detail
	}
	d.Streak = s
	b, err := json.Marshal(d)
	if err != nil {
		return detail // cannot happen for this struct
	}
	return string(b)
}

// RecoveryDetail is the detail of a run.recovered: the streak it ended, or
// "" when there is none to tell.
func RecoveryDetail(s Streak) string {
	if s.FailedRuns <= 0 {
		return ""
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "" // cannot happen for this struct
	}
	return string(b)
}

// failureDetail is the Detail of the run.failed of a run of job that ended in
// status with exitCode and the stored output out, or "" when there is nothing
// to say.
func failureDetail(job Job, status RunStatus, exitCode int, out string, truncated bool) string {
	return encodeFailure(job, status, exitCode, out, truncated, givesVerdict(job.Type))
}

// encodeFailure is failureDetail, with verdictFirst saying whether the output
// opens with its verdict. truncated says the output was cut at 1 MiB (it then
// ends with the truncation marker); the text alone is not trusted to say so.
func encodeFailure(job Job, status RunStatus, exitCode int, out string, truncated, verdictFirst bool) string {
	var d FailureDetail
	if job.Type == HTTP && strings.HasPrefix(out, "HTTP ") {
		d.HTTPStatus, out = exitCode, ""
	}
	if truncated {
		// The cut can fall inside anything, part of a secret printed in a
		// form redaction does not recognise included: its line stays out.
		out, d.Truncated = dropLastLine(strings.TrimSuffix(out, truncationMarker)), true
	}
	if verdictFirst {
		verdict, rest, _ := strings.Cut(strings.TrimLeft(out, "\n"), "\n\n")
		d.Verdict, out = core.HeadLines(verdict, core.MaxOutputTailLines), rest
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

// UnreadableFailureDetail is the Detail of the run.failed recorded for a job
// whose definition cannot be read (FailUnreadableJob), given that run's
// output (UnreadableRunOutput): its FAIL: line as the verdict, the advice
// after it as the output.
func UnreadableFailureDetail(output string) string {
	return encodeFailure(Job{}, StatusFailed, -1, output, false, true)
}

// givesVerdict reports whether a job of type t opens its output with its
// verdict (probeReport.result, probeErrorResult, the drill's report).
func givesVerdict(t JobType) bool {
	switch t {
	case TLS, Domain, Expiry, Drill:
		return true
	}
	return false
}
