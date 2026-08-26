// Package keypool implements the provider key pool: active-window
// selection, round-robin and sticky modes, key health, and exhaustion or
// retirement.
//
// keypool is a leaf package that imports only the standard library and an
// internal clock package. It never sees raw key material in masked form —
// redaction is the dashboard's job, applied over Snapshot output — so it has
// no dependency on redact. Its interface is small: Select, RecordSuccess,
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
	Keys                  []Key
	Mode                  string // "round_robin" or "sticky"
	RequestsPerKey        int    // ignored when Mode is "sticky"
	ActiveWindow          int    // configured value; 0 means "all keys". Kept for snapshots.
	EffectiveActiveWindow int    // resolved window (0=>len(Keys), clamp), read by the pool.
	MaxErrors             int
	Cooldown              time.Duration
	RetireOnExhaustion    bool
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
	// Normalize EffectiveActiveWindow for direct construction (tests, ad-hoc
	// callers): the 0=>len(Keys) rule. In production, config.resolve bakes this
	// value once and app passes it in, so this is a defensive fallback, not the
	// authoritative derivation. ActiveWindow==0 also means "all keys".
	if spec.EffectiveActiveWindow == 0 {
		w := spec.ActiveWindow
		if w == 0 || w > len(spec.Keys) {
			w = len(spec.Keys)
		}
		spec.EffectiveActiveWindow = w
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

// activeWindow returns the effective active window size. The config layer
// resolves the "0 means all keys" rule and the len(keys) clamp once into
// EffectiveActiveWindow, so the pool reads the baked value directly.
func (p *Pool) activeWindow() int {
	return p.spec.EffectiveActiveWindow
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

// Reset clears one key's runtime health state: consecutive-error counter,
// exhaustion timestamp, and retirement. It re-enables an exhausted or retired
// key immediately, bypassing the cooldown. It is a no-op when the key index is
// out of range. Used by the dashboard's per-key "reset" action.
func (p *Pool) Reset(keyNumber int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := keyNumber - 1
	if i < 0 || i >= len(p.spec.Keys) {
		return
	}
	p.consecutiveErrors[i] = 0
	p.exhaustedAt[i] = time.Time{}
	p.retired[i] = false
	p.retiredAt[i] = time.Time{}
}

// State snapshots a single key for persistence or status.
type KeyState struct {
	ConsecutiveErrors int
	ExhaustedAt       time.Time // zero means not exhausted
	Retired           bool
	RetiredAt         time.Time
}

// Classify returns the display state of a key snapshot: "retired",
// "exhausted", or "active". This is the single canonical statement of the
// rule — the same rule isHealthyLocked applies to live pool slots — so callers
// like the dashboard derive the label from one place instead of re-implementing
// the cooldown comparison. exhausted is reported while the key is still within
// its cooldown (now - ExhaustedAt < cooldown); past cooldown it re-enters as
// active (lazy re-entry).
func Classify(ks KeyState, cooldown time.Duration, now time.Time) string {
	if ks.Retired {
		return "retired"
	}
	if !ks.ExhaustedAt.IsZero() && now.Sub(ks.ExhaustedAt) < cooldown {
		return "exhausted"
	}
	return "active"
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
