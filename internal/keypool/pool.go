// Package keypool implements the provider key pool: active-window
// selection, round-robin and sticky modes, key health, and exhaustion or
// retirement.
//
// keypool is a near-leaf package that imports only the standard library and
// redact for masked snapshots. Its interface is small: Select, RecordSuccess,
// RecordError, MarkExhausted, and Snapshot. All of the key pool behavior
// (active window plus FIFO standby promotion, requests_per_key counters,
// lazy cooldown re-entry, and the anti-drain guard) lives behind it.
package keypool

import (
	"errors"
	"sync"
	"time"

	"github.com/not-lucky/conflux/internal/clock"
)

// Spec configures a pool. The fields mirror the resolved config provider
// fields.
type Spec struct {
	Keys               []Key
	Mode               string // "round_robin" or "sticky"
	RequestsPerKey     int    // ignored when Mode is "sticky"
	ActiveWindow       int    // 0 means all healthy
	MaxErrors          int
	Cooldown           time.Duration
	RetireOnExhaustion bool
}

// Key is one provider key, with a value and an optional inline proxy.
type Key struct {
	Value string
	Proxy string
}

// Errors.
var (
	ErrNoHealthyKey = errors.New("no healthy key in active window")
)

// Pool is the per-provider key pool. Pool is safe for concurrent use; all
// state is guarded by a single mutex because selection and health mutate the
// same fields.
type Pool struct {
	clock clock.Clock
	spec  Spec

	mu sync.Mutex
	// per-key state, indexed by declaration order; keyNumber is i+1.
	consecutiveErrors []int
	exhaustedAt       []time.Time // zero means not exhausted
	retired           []bool
	retiredAt         []time.Time

	// selection state
	cursor     int // index of the current slot within the active window
	slotUse    int // requests served on the current slot this rotation
	cycleCount int // incremented on round-robin wrap
	stickyIdx  int // current sticky index when mode is sticky
}

// New builds a Pool. The spec must already be validated, with
// active_window <= len(keys) and so on; config does that.
func New(spec Spec, clk clock.Clock) *Pool {
	if clk == nil {
		clk = clock.RealClock{}
	}
	n := len(spec.Keys)
	return &Pool{
		clock:             clk,
		spec:              spec,
		consecutiveErrors: make([]int, n),
		exhaustedAt:       make([]time.Time, n),
		retired:           make([]bool, n),
		retiredAt:         make([]time.Time, n),
	}
}

// activeWindow returns the configured window size (clamped to len(keys)).
func (p *Pool) activeWindow() int {
	w := p.spec.ActiveWindow
	if w == 0 || w > len(p.spec.Keys) {
		return len(p.spec.Keys)
	}
	return w
}

// Selection is the result of Select.
type Selection struct {
	Key        Key
	KeyNumber  int    // 1-based declaration index
	SlotIndex  int    // 0-based slot index within the active window's healthy list
	Proxy      string // inline proxy (keys[].proxy) carried from the selected key, or empty
	CycleCount int    // per-provider round-robin cycle count, used by proxy rotation
}

// Select picks the next healthy key in the active window per the mode rules.
// Select returns ErrNoHealthyKey when the active window has no healthy key.
// Cooldown re-entry is evaluated lazily here.
func (p *Pool) Select() (Selection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	healthy := p.healthySlots()
	if len(healthy) == 0 {
		return Selection{}, ErrNoHealthyKey
	}

	switch p.spec.Mode {
	case "sticky":
		return p.selectSticky(healthy)
	default:
		return p.selectRoundRobin(healthy)
	}
}

// healthySlots returns the ordered list of healthy slot indices within the
// active window, applying lazy cooldown re-entry. The window is the first N
// healthy keys by declaration order; standby keys are beyond N.
func (p *Pool) healthySlots() []int {
	now := p.clock.Now()
	window := p.activeWindow()
	var out []int
	for i := 0; i < len(p.spec.Keys); i++ {
		if !p.isHealthyLocked(i, now) {
			continue
		}
		out = append(out, i)
		if len(out) >= window {
			break
		}
	}
	return out
}

// isHealthyLocked reports whether key i is eligible for selection: not
// retired, and either not exhausted or past its cooldown, which is the lazy
// re-entry rule.
func (p *Pool) isHealthyLocked(i int, now time.Time) bool {
	if p.retired[i] {
		return false
	}
	if p.exhaustedAt[i].IsZero() {
		return true
	}
	// lazy cooldown re-entry: now - exhaustedAt >= cooldown, inclusive
	return now.Sub(p.exhaustedAt[i]) >= p.spec.Cooldown
}

func (p *Pool) selectRoundRobin(healthy []int) (Selection, error) {
	// Clamp the cursor to the healthy list.
	if p.cursor >= len(healthy) {
		p.cursor = 0
	}
	idx := healthy[p.cursor]
	k := p.spec.Keys[idx]
	// requests_per_key: reuse the slot K times before advancing.
	rpk := p.spec.RequestsPerKey
	if rpk < 1 {
		rpk = 1
	}
	p.slotUse++
	advance := p.slotUse >= rpk
	sel := Selection{Key: k, KeyNumber: idx + 1, SlotIndex: p.cursor, Proxy: k.Proxy}
	if advance {
		p.slotUse = 0
		p.cursor++
		if p.cursor >= len(healthy) {
			p.cursor = 0
			p.cycleCount++
		}
	}
	sel.CycleCount = p.cycleCount
	return sel, nil
}

func (p *Pool) selectSticky(healthy []int) (Selection, error) {
	// sticky: stay on stickyIdx when it is healthy; otherwise jump to the next
	// healthy key by declaration order, wrapping. stickyIdx is a declaration
	// index.
	idx := p.stickyIdx
	if idx >= len(p.spec.Keys) || !p.isHealthyLocked(idx, p.clock.Now()) {
		// jump to first healthy slot, preferring declaration order from idx.
		idx = p.nextHealthyFrom(healthy, idx)
	}
	p.stickyIdx = idx
	// Find the slot position for SlotIndex, used by proxy rotation.
	slot := -1
	for i, h := range healthy {
		if h == idx {
			slot = i
			break
		}
	}
	if slot < 0 {
		slot = 0
	}
	return Selection{Key: p.spec.Keys[idx], KeyNumber: idx + 1, SlotIndex: slot, Proxy: p.spec.Keys[idx].Proxy, CycleCount: p.cycleCount}, nil
}

// nextHealthyFrom returns the next healthy declaration index at or after
// from, wrapping.
func (p *Pool) nextHealthyFrom(healthy []int, from int) int {
	if len(healthy) == 0 {
		return from
	}
	// Prefer the healthy slot whose declaration index is >= from, else wrap.
	for _, h := range healthy {
		if h >= from {
			return h
		}
	}
	return healthy[0]
}

// RecordResult is the outcome of RecordError.
type RecordResult struct {
	Exhausted  bool
	Retired    bool
	ErrorCount int
}

// RecordSuccess resets the key's consecutive-error counter. It is a no-op
// when the key index is out of range, which handles in-flight requests against
// removed keys after a reload.
func (p *Pool) RecordSuccess(keyNumber int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := keyNumber - 1
	if i < 0 || i >= len(p.consecutiveErrors) {
		return
	}
	p.consecutiveErrors[i] = 0
}

// RecordError increments the consecutive-error counter and, when the
// threshold is reached, exhausts or retires the key. It returns the resulting
// state.
func (p *Pool) RecordError(keyNumber int) RecordResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := keyNumber - 1
	if i < 0 || i >= len(p.consecutiveErrors) {
		return RecordResult{}
	}
	p.consecutiveErrors[i]++
	res := RecordResult{ErrorCount: p.consecutiveErrors[i]}
	thresholdReached := p.consecutiveErrors[i] >= p.spec.MaxErrors
	if thresholdReached {
		if p.spec.RetireOnExhaustion {
			p.retired[i] = true
			p.retiredAt[i] = p.clock.Now()
			res.Retired = true
		} else {
			p.exhaustedAt[i] = p.clock.Now()
			res.Exhausted = true
		}
	}
	// In sticky mode, advance the sticky cursor only on a penalized error,
	// which is exhaustion or retirement, not on a sub-threshold increment.
	if p.spec.Mode == "sticky" && thresholdReached {
		p.advanceSticky()
	}
	return res
}

// advanceSticky moves the sticky cursor to the next healthy key past the
// current index, wrapping and skipping exhausted or retired keys.
func (p *Pool) advanceSticky() {
	now := p.clock.Now()
	from := p.stickyIdx + 1
	for n := 0; n < len(p.spec.Keys); n++ {
		idx := from % len(p.spec.Keys)
		if p.isHealthyLocked(idx, now) {
			p.stickyIdx = idx
			return
		}
		from++
	}
	// No healthy key; sticky stays, and Select returns ErrNoHealthyKey.
}

// MarkExhausted immediately exhausts or retires a key, used for
// KEY_AUTH_FATAL. It is a no-op when out of range.
func (p *Pool) MarkExhausted(keyNumber int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := keyNumber - 1
	if i < 0 || i >= len(p.spec.Keys) {
		return
	}
	if p.spec.RetireOnExhaustion {
		p.retired[i] = true
		p.retiredAt[i] = p.clock.Now()
	} else {
		p.exhaustedAt[i] = p.clock.Now()
	}
	if p.spec.Mode == "sticky" {
		p.advanceSticky()
	}
}

// State snapshots a single key for persistence or status.
type KeyState struct {
	ConsecutiveErrors int
	ExhaustedAt       time.Time // zero means not exhausted
	Retired           bool
	RetiredAt         time.Time
}

// Snapshot is the pool state for persistence and /_status.
type Snapshot struct {
	Keys []KeyState
}

// Snapshot returns a copy of the per-key state.
func (p *Pool) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Snapshot{Keys: make([]KeyState, len(p.spec.Keys))}
	for i := range p.spec.Keys {
		s.Keys[i] = KeyState{
			ConsecutiveErrors: p.consecutiveErrors[i],
			ExhaustedAt:       p.exhaustedAt[i],
			Retired:           p.retired[i],
			RetiredAt:         p.retiredAt[i],
		}
	}
	return s
}

// Restore overwrites state from a snapshot, used by persist on startup.
// Only keys whose declaration index still exists are restored; the snapshot is
// keyed by index, but the caller must match by hash first, which is persist's
// job.
func (p *Pool) Restore(s Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < len(p.spec.Keys) && i < len(s.Keys); i++ {
		p.consecutiveErrors[i] = s.Keys[i].ConsecutiveErrors
		p.exhaustedAt[i] = s.Keys[i].ExhaustedAt
		p.retired[i] = s.Keys[i].Retired
		p.retiredAt[i] = s.Keys[i].RetiredAt
	}
}

// CycleCount exposes the per-provider cycle count, used by proxy rotation.
// Sticky pools never advance it.
func (p *Pool) CycleCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cycleCount
}

// Counts returns the gauge counts for /_status and metrics: active,
// standby, exhausted, and retired.
type Counts struct {
	Active, Standby, Exhausted, Retired int
}

func (p *Pool) Counts() Counts {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock.Now()
	window := p.activeWindow()
	healthyCount := 0
	for i := 0; i < len(p.spec.Keys); i++ {
		if p.isHealthyLocked(i, now) {
			healthyCount++
		}
	}
	var c Counts
	c.Active = min(window, healthyCount)
	c.Standby = max(0, healthyCount-window)
	for i := 0; i < len(p.spec.Keys); i++ {
		if p.retired[i] {
			c.Retired++
		} else if !p.exhaustedAt[i].IsZero() && now.Sub(p.exhaustedAt[i]) < p.spec.Cooldown {
			c.Exhausted++
		}
	}
	return c
}
