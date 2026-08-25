// Package ratelimit implements the global per-client-key sliding-window rate
// limiter.
//
// ratelimit is a leaf package. Its interface is Allow(clientKey, limit) bool.
// It uses a 60-second sliding window per exact client key string, an LRU
// bound of 10,000 distinct keys, and evicts idle keys, whose window has
// decayed to zero, first. State is in-memory only.
package ratelimit

import (
	"container/list"
	"sync"
	"time"

	"github.com/not-lucky/conflux/internal/clock"
)

const (
	window        = 60 * time.Second
	maxClientKeys = 10_000
)

type bucket struct {
	key   string
	times []time.Time // timestamps within the window
	el    *list.Element
}

// Limiter is the global rate limiter.
type Limiter struct {
	clock clock.Clock

	mu      sync.Mutex
	buckets map[string]*bucket
	order   *list.List // LRU: front = most recently used
}

// New builds a Limiter.
func New(clk clock.Clock) *Limiter {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &Limiter{
		clock:   clk,
		buckets: map[string]*bucket{},
		order:   list.New(),
	}
}

// Allow reports whether clientKey may proceed under the given RPM limit. It
// records the request by appending a timestamp when allowed. A limit of 0 or
// less means unlimited: Allow always returns true and records nothing.
func (l *Limiter) Allow(clientKey string, limit int) bool {
	if limit <= 0 {
		return true
	}
	now := l.clock.Now()
	cutoff := now.Add(-window)

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[clientKey]
	if !ok {
		b = &bucket{key: clientKey}
		l.buckets[clientKey] = b
		b.el = l.order.PushFront(b)
	} else {
		l.order.MoveToFront(b.el)
	}

	// Drop timestamps outside the window.
	times := b.times
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		times = times[i:]
	}
	// Count requests in the window.
	if len(times) >= limit {
		b.times = times
		// touch LRU even on rejection
		return false
	}
	b.times = append(times, now)
	// Evict idle buckets when over capacity.
	l.evictIdleLocked(now)
	return true
}

// evictIdleLocked drops the oldest idle buckets, those whose window decayed
// to zero, when the bucket count exceeds maxClientKeys, the 10,000 LRU bound.
func (l *Limiter) evictIdleLocked(now time.Time) {
	if len(l.buckets) <= maxClientKeys {
		return
	}
	cutoff := now.Add(-window)
	// From the back, the least recently used, of the LRU, drop idle buckets
	// until the count is at or below capacity.
	for len(l.buckets) > maxClientKeys {
		el := l.order.Back()
		if el == nil {
			break
		}
		b := el.Value.(*bucket)
		// Idle means no requests within the 60s window, so all timestamps are
		// before cutoff.
		idle := true
		for _, ts := range b.times {
			if !ts.Before(cutoff) {
				idle = false
				break
			}
		}
		if !idle {
			// The LRU back is not idle; evicting it would break the
			// idle-first rule. Stop to avoid evicting an active key.
			break
		}
		l.order.Remove(el)
		delete(l.buckets, b.key)
	}
}

// Snapshot returns the current count of tracked client keys, for /_status.
func (l *Limiter) Snapshot() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
