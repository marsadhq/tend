package jobs

import (
	"testing"
	"time"
)

// TestFinishRetryPauses pins the shape of the default retry schedule for a
// failed finish: a few attempts, each pause longer than the one before, and the
// whole series short enough that a run being retried is still far from the
// reaper's slack.
func TestFinishRetryPauses(t *testing.T) {
	if n := len(finishRetryPauses); n < 2 || n > 5 {
		t.Fatalf("%d retries, want a few (2 to 5)", n)
	}
	r := &Runner{}
	var total, prev time.Duration
	for i, want := range finishRetryPauses {
		got := r.finishBackoff(i + 1)
		if got != want {
			t.Errorf("finishBackoff(%d) = %s, want %s", i+1, got, want)
		}
		if got <= prev {
			t.Errorf("pause %d is %s, not longer than the %s before it", i+1, got, prev)
		}
		prev = got
		total += got
	}
	if total >= reapSlack/2 {
		t.Errorf("pauses add up to %s, want well under reapSlack (%s)", total, reapSlack)
	}

	r.FinishBackoff = func(attempt int) time.Duration { return time.Duration(attempt) * time.Hour }
	if got := r.finishBackoff(2); got != 2*time.Hour {
		t.Errorf("finishBackoff(2) with FinishBackoff set = %s, want 2h", got)
	}
}
