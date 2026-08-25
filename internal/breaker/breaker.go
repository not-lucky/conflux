// Package breaker implements the per-provider upstream 5xx circuit
// breaker.
//
// breaker is a leaf package. Its interface is On5xx, On2xx, and Open. The
// breaker limits load amplification when an upstream returns 5xx across the
// board: after upstream_5xx_threshold consecutive 5xx responses, it opens for
// upstream_5xx_cooldown. While open, 5xx retries are skipped. A non-5xx
// response such as 2xx closes and resets the breaker immediately.
package breaker

import (
	"sync"
	"time"

	"github.com/not-lucky/conflux/internal/clock"
)

// Breaker is the per-provider upstream 5xx circuit breaker.
type Breaker struct {
	clock     clock.Clock
	threshold int
	cooldown  time.Duration

	mu             sync.Mutex
	consecutive5xx int
	openUntil      time.Time
}

// New builds a Breaker with the given threshold and cooldown.
func New(threshold int, cooldown time.Duration, clk clock.Clock) *Breaker {
	if clk == nil {
		clk = clock.RealClock{}
	}
	if threshold < 1 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Breaker{clock: clk, threshold: threshold, cooldown: cooldown}
}

// On5xx records a 5xx response. When the consecutive-5xx count reaches the
// threshold, the breaker opens for the cooldown. On5xx returns true when this
// call opens the breaker.
func (b *Breaker) On5xx() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive5xx++
	if b.consecutive5xx >= b.threshold {
		b.openUntil = b.clock.Now().Add(b.cooldown)
		return true
	}
	return false
}

// On2xx closes and resets the breaker immediately on a 2xx or any non-5xx
// success.
func (b *Breaker) On2xx() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive5xx = 0
	b.openUntil = time.Time{}
}

// Open reports whether the breaker is currently open, in which case 5xx
// retries are skipped. After the cooldown elapses, the breaker is half-open
// and Open returns false, letting the next request through.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return false
	}
	now := b.clock.Now()
	if now.Before(b.openUntil) {
		return true
	}
	// Cooldown elapsed: half-open. Clear the trip so the next 5xx reopens from
	// the threshold rather than a stale openUntil.
	b.openUntil = time.Time{}
	return false
}
