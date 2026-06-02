package clock

import (
	"sync"
	"time"
)

// Clock is the minimal time-source interface used throughout Tend.
type Clock interface {
	Now() time.Time
}

// RealClock implements Clock using the system clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// FakeClock implements Clock with a manually-controlled time value.
// It is safe for concurrent use.
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake returns a FakeClock initialised to now.
func NewFake(now time.Time) *FakeClock { return &FakeClock{t: now} }

// Now returns the fake clock's current time.
func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance moves the fake clock forward by d.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}
