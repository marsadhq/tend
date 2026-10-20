package jobs

import (
	"testing"
	"time"
)

func TestDurationText(t *testing.T) {
	cases := map[time.Duration]string{
		48 * time.Hour:                       "48h",
		14*time.Hour + 24*time.Minute:        "14h24m",
		90 * time.Second:                     "1m30s",
		10 * time.Minute:                     "10m",
		1500 * time.Millisecond:              "1.5s",
		2*time.Hour + 3*time.Second:          "2h0m3s",
		12345678 * time.Microsecond:          "12.346s",
		3*time.Minute + 400*time.Millisecond: "3m",
	}
	for d, want := range cases {
		if got := durationText(d); got != want {
			t.Errorf("durationText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestHumanDaysAndFloorDays(t *testing.T) {
	if got := humanDays(14 * day); got != "14 days" {
		t.Errorf("humanDays(14d) = %q", got)
	}
	if got := humanDays(-3 * day); got != "3 days" {
		t.Errorf("humanDays(-3d) = %q", got)
	}
	if got := humanDays(26 * time.Hour); got != "26 hours" {
		t.Errorf("humanDays(26h) = %q", got)
	}
	if got := humanDays(time.Hour + time.Minute); got != "1 hour" {
		t.Errorf("humanDays(61m) = %q", got)
	}
	if got := humanDays(59 * time.Minute); got != "59 minutes" {
		t.Errorf("humanDays(59m) = %q", got)
	}
	if got := humanDays(-30 * time.Second); got != "0 minutes" {
		t.Errorf("humanDays(-30s) = %q", got)
	}
	if got := floorDays(-time.Hour); got != -1 {
		t.Errorf("floorDays(-1h) = %d, want -1", got)
	}
	if got := floorDays(day - time.Second); got != 0 {
		t.Errorf("floorDays(23h59m59s) = %d, want 0", got)
	}
}
