// Package proxy implements the proxy subsystem: pool resolution,
// rotation, and per-URL health with circuit breaking.
//
// proxy is a near-leaf package that imports the standard library and redact.
// The interface splits cleanly:
//   - Resolver picks a proxy or direct for a given key, applying the
//     resolution order (inline, then provider, then global, then direct) with
//     health filtering at each step.
//   - Health records proxy errors and successes, and trips and recovers the
//     global per-URL circuit breaker.
//
// The actual HTTP transport lives in the forward package; proxy only decides
// which URL to use and whether it is healthy.
package proxy

import (
	"sync"
	"time"

	"github.com/not-lucky/conflux/internal/clock"
)

// PoolConfig is a proxy pool with its rotation and breaker knobs.
type PoolConfig struct {
	URLs           []string
	RotateInterval int // 0 means no rotation, so pinning is sticky
	MaxErrors      int
	Cooldown       time.Duration
}

// Health tracks the global per-URL circuit breaker state. Health is global
// per URL: tripping http://p in one provider excludes it everywhere.
type Health struct {
	clock clock.Clock

	mu        sync.Mutex
	errs      map[string]int       // consecutiveErrors per URL
	deadUntil map[string]time.Time // tripped-until per URL
	lastError map[string]string
}

// NewHealth builds an empty global health tracker.
func NewHealth(clk clock.Clock) *Health {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &Health{
		clock:     clk,
		errs:      map[string]int{},
		deadUntil: map[string]time.Time{},
		lastError: map[string]string{},
	}
}

// RecordError increments a proxy's consecutive-error counter and trips the
// breaker when it reaches the per-pool threshold. The threshold is passed by
// the caller because it depends on which pool selected the proxy: the
// effective threshold is per-pool, else global.
func (h *Health) RecordError(url string, maxErrors int, cooldown time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if url == "" {
		return
	}
	h.errs[url]++
	if h.errs[url] >= maxErrors && maxErrors > 0 {
		h.deadUntil[url] = h.clock.Now().Add(cooldown)
	}
}

// RecordSuccess resets a proxy's breaker, on any successful upstream
// response through the proxy, including a non-2xx that is not a transport
// failure.
func (h *Health) RecordSuccess(url string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if url == "" {
		return
	}
	delete(h.errs, url)
	delete(h.deadUntil, url)
	delete(h.lastError, url)
}

// SetLastError stores the last transport message for the URL, for /_status.
func (h *Health) SetLastError(url, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if url == "" {
		return
	}
	h.lastError[url] = msg
}

// isHealthyLocked reports whether a URL is past its trip window.
func (h *Health) isHealthyLocked(url string, now time.Time) bool {
	du, ok := h.deadUntil[url]
	if !ok {
		return true
	}
	return now.After(du) || now.Equal(du) // now >= deadUntil
}

// Healthy reports whether a single URL is currently healthy.
func (h *Health) Healthy(url string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.isHealthyLocked(url, h.clock.Now())
}

// filterHealthy returns the subset of urls that are not currently tripped,
// preserving order.
func (h *Health) filterHealthy(urls []string, now time.Time) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if h.isHealthyLocked(u, now) {
			out = append(out, u)
		}
	}
	return out
}

// SnapshotEntry is one URL's health for /_status.
type SnapshotEntry struct {
	URL               string
	Healthy           bool
	ConsecutiveErrors int
	DeadUntil         time.Time // zero means not tripped
	LastError         string
}

// Snapshot returns health state for a set of URLs, deduplicated and with
// order preserved.
func (h *Health) Snapshot(urls []string) []SnapshotEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.clock.Now()
	seen := map[string]bool{}
	out := make([]SnapshotEntry, 0, len(urls))
	for _, u := range urls {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, SnapshotEntry{
			URL:               u,
			Healthy:           h.isHealthyLocked(u, now),
			ConsecutiveErrors: h.errs[u],
			DeadUntil:         h.deadUntil[u],
			LastError:         h.lastError[u],
		})
	}
	return out
}

// Restore overwrites breaker state from persisted entries, used by the app
// on startup. Only entries with a non-zero DeadUntil restore the trip; a zero
// DeadUntil restores only the consecutive-error counter. Empty URLs are
// skipped. The Healthy field is informational here; the live isHealthyLocked
// re-evaluates against the clock.
func (h *Health) Restore(entries []SnapshotEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range entries {
		if e.URL == "" {
			continue
		}
		if e.ConsecutiveErrors > 0 {
			h.errs[e.URL] = e.ConsecutiveErrors
		}
		if !e.DeadUntil.IsZero() {
			h.deadUntil[e.URL] = e.DeadUntil
		}
	}
}

// Selection is the result of Resolve.
type Selection struct {
	URL    string // empty means direct, no proxy
	Number int    // 1-based index within the healthy pool used; 0 if direct
	Direct bool
}

// Resolver decides which proxy to use for a given key slot. It is
// constructed per request attempt with the key's inline proxy when present,
// the provider pool when present, and the global pool, plus the active-window
// slot index and the per-provider cycle count for the rotation formula.
type Resolver struct {
	Health       *Health
	Inline       string      // keys[].proxy for the selected key, or empty
	ProviderPool *PoolConfig // provider-scoped pool, or nil
	GlobalPool   *PoolConfig // global pool
	CycleCount   int         // per-provider cycle count
}

// Resolve applies the resolution order with health filtering and the rotation
// formula. When all proxies are tripped at any level, Resolve falls through to
// direct.
func (r Resolver) Resolve(slotIndex int) Selection {
	now := r.Health.clock.Now()

	// 1. Inline, a single proxy. When tripped, fall back to direct.
	if r.Inline != "" {
		if r.Health.isHealthyLocked(r.Inline, now) {
			return Selection{URL: r.Inline, Number: 1, Direct: false}
		}
		// A tripped inline falls to direct for this attempt.
		return Selection{Direct: true}
	}

	// 2. Provider pool.
	if r.ProviderPool != nil && len(r.ProviderPool.URLs) > 0 {
		healthy := r.Health.filterHealthy(r.ProviderPool.URLs, now)
		if len(healthy) > 0 {
			url, num := assign(healthy, slotIndex, r.ProviderPool.RotateInterval, r.CycleCount)
			return Selection{URL: url, Number: num, Direct: false}
		}
		// All provider-pool proxies tripped: fall through to global.
	}

	// 3. Global pool.
	if r.GlobalPool != nil && len(r.GlobalPool.URLs) > 0 {
		healthy := r.Health.filterHealthy(r.GlobalPool.URLs, now)
		if len(healthy) > 0 {
			url, num := assign(healthy, slotIndex, r.GlobalPool.RotateInterval, r.CycleCount)
			return Selection{URL: url, Number: num, Direct: false}
		}
		// All global proxies tripped: go direct.
	}

	// 4. Direct, no proxy.
	return Selection{Direct: true}
}

// assign applies the rotation formula:
//
//	shift = floor(cycleCount / rotateInterval)   when rotateInterval is set; else 0
//	index = (slotIndex + shift) % len(healthyPool)
//
// assign returns the URL and its 1-based index within the healthy pool.
func assign(healthy []string, slotIndex, rotateInterval, cycleCount int) (string, int) {
	n := len(healthy)
	if n == 0 {
		return "", 0
	}
	shift := 0
	if rotateInterval > 0 {
		shift = cycleCount / rotateInterval
	}
	idx := ((slotIndex % n) + shift) % n
	if idx < 0 {
		idx += n
	}
	return healthy[idx], idx + 1
}
