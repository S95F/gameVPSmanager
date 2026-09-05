package console

import "time"

// Clock is the seam that makes two timing rules testable without sleeping:
// the 250 ms partial-line flush (section 11.3 rule 5) and the 2 s log-buffer
// flush (section 11.3, persistence). Both are asserted against FakeClock, so
// the suite has no sleeps in it and no flake budget to spend.
//
// AfterFunc rather than a ticker channel, because both consumers want a timer
// they can stop the moment the condition that armed it disappears -- a ticker
// firing into an idle console every 250 ms forever is a wakeup per instance
// per quarter second on a box running twenty of them.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is the subset of *time.Timer this package uses.
type Timer interface {
	Stop() bool
	Reset(d time.Duration) bool
}

// SystemClock is the real clock. The zero value is ready to use.
type SystemClock struct{}

// Now reports the current wall time.
func (SystemClock) Now() time.Time { return time.Now() }

// AfterFunc schedules f on its own goroutine, as time.AfterFunc does.
func (SystemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// clockOr returns c, or the system clock when c is nil.
func clockOr(c Clock) Clock {
	if c == nil {
		return SystemClock{}
	}
	return c
}
