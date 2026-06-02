package clock

import (
	"testing"
	"time"
)

// Compile-time interface satisfaction assertions.
var _ Clock = RealClock{}
var _ Clock = (*FakeClock)(nil)

func TestFakeClock_Now_returnsSetTime(t *testing.T) {
	t0 := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	fc := NewFake(t0)
	if got := fc.Now(); !got.Equal(t0) {
		t.Fatalf("Now() = %v, want %v", got, t0)
	}
}

func TestFakeClock_Advance_single(t *testing.T) {
	t0 := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	fc := NewFake(t0)
	d := 5 * time.Minute
	fc.Advance(d)
	want := t0.Add(d)
	if got := fc.Now(); !got.Equal(want) {
		t.Fatalf("After Advance(%v): Now() = %v, want %v", d, got, want)
	}
}

func TestFakeClock_Advance_accumulates(t *testing.T) {
	t0 := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	fc := NewFake(t0)
	fc.Advance(1 * time.Hour)
	fc.Advance(30 * time.Minute)
	want := t0.Add(90 * time.Minute)
	if got := fc.Now(); !got.Equal(want) {
		t.Fatalf("After two Advances: Now() = %v, want %v", got, want)
	}
}

func TestRealClock_Now_returnsApproximateCurrentTime(t *testing.T) {
	before := time.Now()
	rc := RealClock{}
	got := rc.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("RealClock.Now() = %v, expected between %v and %v", got, before, after)
	}
}
