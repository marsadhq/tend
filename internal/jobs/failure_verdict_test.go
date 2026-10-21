package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// decodeFailure decodes the detail failureDetail gives res's run of j.
func decodeFailure(t *testing.T, j Job, res RunResult) FailureDetail {
	t.Helper()
	var d FailureDetail
	if raw := failureDetail(j, res.Status, res.ExitCode, res.Output, res.Truncated); raw != "" {
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// A probe or a drill gives its verdict in its first lines (a FAIL: line per
// problem, or why it could not check) and then what it checked, cleaned up
// and printed: a tail of the output alone would show only that. Their
// failure's detail keeps the verdict on its own, and the tail of what follows
// it, so the alert says why the run failed.
func TestFailureDetailKeepsAProbesVerdict(t *testing.T) {
	ca := newTestCA(t)
	rdap, _, domainEx := domainSetup(t)
	rdap.answers["example.test"] = rdapAnswer("example.test",
		map[string]string{"expiration": time.Now().Add(20 * day).UTC().Format(time.RFC3339)})
	drill, _ := drillJob(t, Spec{
		Restore: `for i in $(seq 1 30); do echo "restoring table $i"; done`,
		Checks:  []string{`for i in $(seq 1 20); do echo "checking row $i"; done; false`},
		Cleanup: `for i in $(seq 1 15); do echo "cleaning $i"; done`,
	})

	for _, tc := range []struct {
		name    string
		job     Job
		ex      *Executor
		verdict string // the detail's verdict starts with it
		tail    string // and its output ends with it
	}{
		{"tls", tlsJob(tlsServer(t, ca.issue(t, time.Now().Add(-80*day), time.Now().Add(5*day), "tend.test")),
			Spec{ServerName: "tend.test", CAFile: ca.writePEM(t), WarnDays: 14}), NewExecutor(),
			"FAIL: certificate expires in 4 days", "verified (2 sent)"},
		{"domain", domainJob("example.test", 30), domainEx, "FAIL: registration expires in 19 days", ""},
		{"expiry", Job{Name: "token", Type: Expiry, Spec: &Spec{ExpiresAt: time.Now().Add(3 * day), WarnDays: 14}},
			NewExecutor(), "FAIL: expires in 2 days", "days_left:   2"},
		{"expiry command", Job{Name: "key", Type: Expiry, TimeoutSeconds: 5,
			Spec: &Spec{ExpiryCommand: `echo "no key here" >&2; exit 4`, WarnDays: 7}},
			NewExecutor(), "error: expiry_command exited with status 4", "no key here"},
		{"drill", drill, NewExecutor(), "FAIL: check 1 failed (exit 1)", "cleaning 15"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.ex.Run(context.Background(), tc.job, nil)
			if res.Status != StatusFailed {
				t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
			}
			d := decodeFailure(t, tc.job, res)
			if !strings.HasPrefix(d.Verdict, tc.verdict) || strings.Contains(d.Verdict, "\n\n") {
				t.Errorf("verdict = %q, want the first paragraph, starting %q", d.Verdict, tc.verdict)
			}
			if strings.Contains(d.Output, tc.verdict) {
				t.Errorf("output %q repeats the verdict", d.Output)
			}
			if !strings.HasSuffix(d.Output, tc.tail) {
				t.Errorf("output = %q, want the tail of what follows the verdict, ending %q", d.Output, tc.tail)
			}
		})
	}

	// A shell job has no verdict: its tail is its output's end.
	shell := Job{Type: Shell}
	if d := decodeFailure(t, shell, RunResult{Status: StatusFailed, Output: "FAIL: x\n\nmore"}); d.Verdict != "" || d.Output != "FAIL: x\n\nmore" {
		t.Errorf("shell job detail = %+v, want no verdict", d)
	}
}
