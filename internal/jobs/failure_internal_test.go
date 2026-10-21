package jobs

import (
	"encoding/json"
	"strings"
	"testing"
)

// A run whose output was cut at 1 MiB has its alert tail taken from before
// the cut, without the line the cut fell in: whatever was cut there (part of
// a secret printed in a form redaction does not recognise, say) stays out of
// the alert. The detail says the tail is not the true end of the output.
func TestFailureDetailOfATruncatedRun(t *testing.T) {
	job := Job{Type: Shell}
	for _, tc := range []struct {
		name, out, wantOutput string
		wantTruncated         bool
	}{
		{"whole", "line 1\nline 2\nboom", "line 1\nline 2\nboom", false},
		{"cut", "line 1\nline 2\npartial-tok_0123" + truncationMarker, "line 1\nline 2", true},
		{"cut in its only line", strings.Repeat("x", 100) + truncationMarker, "", true},
	} {
		var d FailureDetail
		if raw := failureDetail(job, StatusFailed, tc.out); raw != "" {
			if err := json.Unmarshal([]byte(raw), &d); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		if d.Output != tc.wantOutput || d.Truncated != tc.wantTruncated {
			t.Errorf("%s: detail = %+v, want output %q, truncated %v", tc.name, d, tc.wantOutput, tc.wantTruncated)
		}
	}
}
