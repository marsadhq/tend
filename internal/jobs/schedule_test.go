package jobs

import (
	"testing"
	"time"
)

func TestNextRunCron(t *testing.T) {
	base := time.Date(2026, 5, 29, 2, 0, 0, 0, time.UTC)
	j := Job{Cron: "0 3 * * *"} // 03:00 daily
	got, err := j.NextRun(base)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("want %v got %v", want, got)
	}
}

func TestNextRunInterval(t *testing.T) {
	base := time.Date(2026, 5, 29, 2, 0, 0, 0, time.UTC)
	j := Job{IntervalSeconds: 900} // every 15m
	got, err := j.NextRun(base)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(base.Add(15 * time.Minute)) {
		t.Fatalf("got %v", got)
	}
}

func TestNextRunOneOffInPastReturnsZero(t *testing.T) {
	base := time.Date(2026, 5, 29, 2, 0, 0, 0, time.UTC)
	j := Job{RunAt: base.Add(-time.Hour)}
	got, err := j.NextRun(base)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("expired one-off should be zero, got %v", got)
	}
}

// TestNextRunNoScheduleError verifies that a Job with no schedule fields set
// returns a non-nil error.
func TestNextRunNoScheduleError(t *testing.T) {
	j := Job{}
	_, err := j.NextRun(time.Now())
	if err == nil {
		t.Fatal("expected non-nil error for job with no schedule")
	}
}

// TestNextRunInvalidCronError verifies that an invalid cron expression returns
// a non-nil error (without asserting the exact message).
func TestNextRunInvalidCronError(t *testing.T) {
	j := Job{Cron: "not a cron"}
	_, err := j.NextRun(time.Now())
	if err == nil {
		t.Fatal("expected non-nil error for invalid cron expression")
	}
}

// TestNextRunOneOffInFuture verifies that a one-off job whose RunAt is in the
// future returns exactly RunAt.
func TestNextRunOneOffInFuture(t *testing.T) {
	base := time.Date(2026, 5, 29, 2, 0, 0, 0, time.UTC)
	runAt := base.Add(time.Hour)
	j := Job{RunAt: runAt}
	got, err := j.NextRun(base)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(runAt) {
		t.Fatalf("want %v got %v", runAt, got)
	}
}

// TestNextRunDST verifies that a daily cron at noon respects local wall-clock
// time across a DST spring-forward boundary.
//
// US spring-forward 2026 is March 8: clocks jump from 02:00 to 03:00.
// We use noon (12:00) specifically because 02:00 is the nonexistent hour during
// spring-forward and has implementation-defined handling; noon is unambiguous on
// both sides of the transition.
//
// A base of 2026-03-07 13:00 America/New_York (just after noon on the day
// before spring-forward) with cron "0 12 * * *" should yield 2026-03-08 12:00
// local time - confirming the schedule advances a full wall-clock day and
// preserves the 12:00 local hour even though the UTC offset changes overnight.
func TestNextRunDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tz database unavailable:", err)
	}

	// base is 2026-03-07 13:00 ET (EST, UTC-5): just after noon on the day
	// before spring-forward.
	base := time.Date(2026, 3, 7, 13, 0, 0, 0, loc)

	j := Job{Cron: "0 12 * * *"} // noon daily
	got, err := j.NextRun(base)
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 3, 8, 12, 0, 0, 0, loc) // noon on spring-forward day (EDT, UTC-4)

	if !got.Equal(want) {
		t.Fatalf("want %v got %v", want, got)
	}
	// Verify wall-clock hour in local time is 12 (not shifted by DST change).
	gotLocal := got.In(loc)
	if gotLocal.Hour() != 12 {
		t.Fatalf("expected local hour 12 in %s, got %d (full time: %v)", loc, gotLocal.Hour(), gotLocal)
	}
	// Must be strictly after base.
	if !got.After(base) {
		t.Fatalf("next run %v should be after base %v", got, base)
	}
}
