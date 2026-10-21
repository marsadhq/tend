package httpserver

import (
	"testing"
	"time"
)

func TestRelTime(t *testing.T) {
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "just now"},
		{-30 * time.Second, "just now"},
		{30 * time.Second, "in under a minute"},
		{-4 * time.Minute, "4m ago"},
		{59*time.Minute + 59*time.Second, "in 59m"},
		{time.Hour, "in 1h"},
		{-(3*time.Hour + 5*time.Minute), "3h 5m ago"},
		{-3 * time.Hour, "3h ago"},
		{9*time.Hour + 49*time.Minute, "in 9h 49m"},
		{14*time.Hour + 30*time.Minute, "in 14h"},
		{-26 * time.Hour, "1d 2h ago"},
		{48 * time.Hour, "in 2d"},
		{5*24*time.Hour + 3*time.Hour, "in 5d"},
		{59 * 24 * time.Hour, "in 59d"},
		{-100 * 24 * time.Hour, "2026-07-13"},
		{3 * 365 * 24 * time.Hour, "2029-10-20"},
	} {
		if got := relTime(now, now.Add(tc.d)); got != tc.want {
			t.Errorf("relTime(now%+v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestWhenOf(t *testing.T) {
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	if w := whenOf(now, time.Time{}); w != nil {
		t.Errorf("whenOf(zero) = %+v, want nil", w)
	}
	w := whenOf(now, now.Add(-4*time.Minute))
	if w == nil || w.Rel != "4m ago" || w.Abs != "2026-10-21 13:56:00 UTC" || w.ISO != "2026-10-21T13:56:00Z" {
		t.Errorf("whenOf(4m ago) = %+v", w)
	}
}

func TestNextRunOf(t *testing.T) {
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	soon := now.Add(9*time.Hour + 49*time.Minute)
	for _, tc := range []struct {
		name    string
		enabled bool
		next    time.Time
		want    string // "" for none
	}{
		{"scheduled", true, soon, "in 9h 49m"},
		{"paused jobs do not run", false, soon, ""},
		{"manual", true, time.Time{}, ""},
		{"overdue", true, now.Add(-5 * time.Minute), "due"},
		{"due this instant", true, now, "due"},
	} {
		got := nextRunOf(now, tc.enabled, tc.next)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: got %+v, want none", tc.name, got)
		case tc.want != "" && (got == nil || got.Rel != tc.want):
			t.Errorf("%s: got %+v, want %q", tc.name, got, tc.want)
		}
	}
}
