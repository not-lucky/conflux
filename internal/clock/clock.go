// Package clock provides the shared Clock interface injected into the
// leaf packages (keypool, proxy, ratelimit, breaker). Tests pass a fake
// clock; production passes a RealClock, which wraps time.Now.
package clock

import "time"

// Clock is the injected time source.
type Clock interface {
	Now() time.Time
}

// RealClock returns wall-clock time.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }
