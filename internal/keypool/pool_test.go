package keypool

import (
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time      { return f.t }
func (f *fakeClock) Add(d time.Duration) { f.t = f.t.Add(d) }

func keys(n int) []Key {
	out := make([]Key, n)
	for i := range out {
		out[i] = Key{Value: string(rune('A' + i))}
	}
	return out
}

// TestRoundRobinGoldenTable reproduces the deterministic example: an
// active_window of 3, keys [A,B,C], and K=2.
func TestRoundRobinGoldenTable(t *testing.T) {
	p := New(Spec{
		Keys:           keys(3),
		Mode:           "round_robin",
		RequestsPerKey: 2,
		ActiveWindow:   3,
		MaxErrors:      5,
		Cooldown:       5 * time.Hour,
	}, &fakeClock{t: time.Unix(1000, 0)})

	want := []string{"A", "A", "B", "B", "C", "C", "A"}
	for i, w := range want {
		s, err := p.Select()
		if err != nil {
			t.Fatalf("req %d: %v", i+1, err)
		}
		if s.Key.Value != w {
			t.Errorf("req %d: got %q, want %q", i+1, s.Key.Value, w)
		}
	}
}

// TestRoundRobinJumpOnExhaust verifies that when B exhausts after request 3,
// request 4 jumps to C with counter 1.
func TestRoundRobinJumpOnExhaust(t *testing.T) {
	p := New(Spec{
		Keys:           keys(3),
		Mode:           "round_robin",
		RequestsPerKey: 2,
		ActiveWindow:   3,
		MaxErrors:      5,
		Cooldown:       5 * time.Hour,
	}, &fakeClock{t: time.Unix(1000, 0)})

	// req1=A, req2=A (advance), req3=B
	p.Select()          // A
	p.Select()          // A advances to B
	s3, _ := p.Select() // B with counter 1
	if s3.Key.Value != "B" {
		t.Fatalf("req3 = %q, want B", s3.Key.Value)
	}
	// Now exhaust B by recording max_errors errors.
	for i := 0; i < p.spec.MaxErrors; i++ {
		p.RecordError(s3.KeyNumber)
	}
	// Request 4 should jump to C with counter 1.
	s4, err := p.Select()
	if err != nil {
		t.Fatalf("req4: %v", err)
	}
	if s4.Key.Value != "C" {
		t.Errorf("req4 = %q, want C (jump on B exhausted)", s4.Key.Value)
	}
}

func TestStickyStaysUntilPenalized(t *testing.T) {
	p := New(Spec{
		Keys:         keys(3),
		Mode:         "sticky",
		ActiveWindow: 3,
		MaxErrors:    3,
		Cooldown:     5 * time.Hour,
	}, &fakeClock{t: time.Unix(1000, 0)})

	// Sticky stays on A.
	for i := 0; i < 5; i++ {
		s, _ := p.Select()
		if s.Key.Value != "A" {
			t.Fatalf("req %d: sticky = %q, want A", i+1, s.Key.Value)
		}
	}
	// A non-penalized retry, modeled as a RecordError with the threshold not
	// reached, does not advance sticky. Record 2 errors, below max=3, so sticky
	// stays.
	p.RecordError(1)
	p.RecordError(1)
	s, _ := p.Select()
	if s.Key.Value != "A" {
		t.Errorf("after 2 errors sticky = %q, want A (below threshold)", s.Key.Value)
	}
	// A 3rd error exhausts the key and advances sticky to the next healthy
	// key, B.
	res := p.RecordError(1)
	if !res.Exhausted {
		t.Errorf("expected exhaustion on 3rd error, got %+v", res)
	}
	s, _ = p.Select()
	if s.Key.Value != "B" {
		t.Errorf("after penalized exhaust sticky = %q, want B", s.Key.Value)
	}
}

func TestNoHealthyKey(t *testing.T) {
	p := New(Spec{
		Keys:         keys(2),
		Mode:         "round_robin",
		ActiveWindow: 2,
		MaxErrors:    1,
		Cooldown:     5 * time.Hour,
	}, &fakeClock{t: time.Unix(1000, 0)})
	// Exhaust both.
	p.RecordError(1) // A exhausted (max=1)
	p.RecordError(2) // B exhausted
	_, err := p.Select()
	if !errors.Is(err, ErrNoHealthyKey) {
		t.Errorf("err = %v, want ErrNoHealthyKey", err)
	}
}

func TestCooldownReentry(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	p := New(Spec{
		Keys:         keys(2),
		Mode:         "round_robin",
		ActiveWindow: 2,
		MaxErrors:    1,
		Cooldown:     1 * time.Hour,
	}, clk)
	// Exhaust A (max=1). B remains healthy.
	p.RecordError(1)
	// Select should skip exhausted A and return B.
	s, err := p.Select()
	if err != nil || s.Key.Value != "B" {
		t.Fatalf("after A exhausted, Select = %v %v, want B", s, err)
	}
	// Before cooldown, A is not healthy; with both considered, only B is
	// healthy. Advance time past cooldown so A re-enters.
	clk.Add(2 * time.Hour)
	// Now Select should be able to return A again within round-robin. The
	// exact slot is not asserted, but A must be healthy in a fresh pool
	// equivalent.
	c := p.Counts()
	if c.Exhausted != 0 {
		t.Errorf("after cooldown, exhausted count = %d, want 0", c.Exhausted)
	}
}

func TestRetireOnExhaustion(t *testing.T) {
	p := New(Spec{
		Keys:               keys(2),
		Mode:               "round_robin",
		ActiveWindow:       2,
		MaxErrors:          1,
		Cooldown:           1 * time.Hour,
		RetireOnExhaustion: true,
	}, &fakeClock{t: time.Unix(1000, 0)})
	res := p.RecordError(1)
	if !res.Retired {
		t.Fatalf("expected retirement, got %+v", res)
	}
	c := p.Counts()
	if c.Retired != 1 {
		t.Errorf("retired count = %d, want 1", c.Retired)
	}
	// Advance time: retired keys do NOT re-enter.
	clk := p.clock.(*fakeClock)
	clk.Add(10 * time.Hour)
	c = p.Counts()
	if c.Retired != 1 {
		t.Errorf("retired count after time = %d, want 1 (never recovers)", c.Retired)
	}
}

func TestMarkExhaustedImmediate(t *testing.T) {
	p := New(Spec{
		Keys:         keys(2),
		Mode:         "round_robin",
		ActiveWindow: 2,
		MaxErrors:    5, // high threshold; markExhausted bypasses it
		Cooldown:     5 * time.Hour,
	}, &fakeClock{t: time.Unix(1000, 0)})
	p.MarkExhausted(1) // immediate exhaustion, the KEY_AUTH_FATAL path
	c := p.Counts()
	if c.Exhausted != 1 {
		t.Errorf("exhausted = %d, want 1 after MarkExhausted", c.Exhausted)
	}
}

// TestInlineProxyCarriedInSelection verifies that a key's inline proxy
// (keys[].proxy) is copied into the returned Selection, for both the key that
// has a proxy and keys without one. A pool with 2 keys where key #2 has an
// inline proxy must surface that proxy on Selection.Proxy when key #2 is
// selected.
func TestInlineProxyCarriedInSelection(t *testing.T) {
	twoKeys := []Key{
		{Value: "A"}, // no inline proxy
		{Value: "B", Proxy: "http://inline:8080"}, // inline proxy
	}
	p := New(Spec{
		Keys:           twoKeys,
		Mode:           "round_robin",
		RequestsPerKey: 1,
		ActiveWindow:   2,
		MaxErrors:      5,
		Cooldown:       5 * time.Hour,
	}, &fakeClock{t: time.Unix(1000, 0)})

	// First select lands on key A (slot 0), which has no inline proxy.
	s1, err := p.Select()
	if err != nil {
		t.Fatalf("select 1: %v", err)
	}
	if s1.Key.Value != "A" {
		t.Fatalf("select 1 key = %q, want A", s1.Key.Value)
	}
	if s1.Proxy != "" {
		t.Errorf("select 1 Proxy = %q, want empty (key A has no inline proxy)", s1.Proxy)
	}

	// Second select advances to key B (slot 1), which carries the inline proxy.
	s2, err := p.Select()
	if err != nil {
		t.Fatalf("select 2: %v", err)
	}
	if s2.Key.Value != "B" {
		t.Fatalf("select 2 key = %q, want B", s2.Key.Value)
	}
	if s2.Proxy != "http://inline:8080" {
		t.Errorf("select 2 Proxy = %q, want http://inline:8080", s2.Proxy)
	}
}

func TestInlineProfileCarriedInSelection(t *testing.T) {
	twoKeys := []Key{
		{Value: "A", Profile: "cursor"},
		{Value: "B", Profile: "opencode"},
	}
	p := New(Spec{
		Keys:           twoKeys,
		Mode:           "round_robin",
		RequestsPerKey: 1,
		ActiveWindow:   2,
		MaxErrors:      5,
		Cooldown:       5 * time.Hour,
	}, &fakeClock{t: time.Unix(1000, 0)})

	s1, err := p.Select()
	if err != nil {
		t.Fatalf("select 1: %v", err)
	}
	if s1.Profile != "cursor" {
		t.Errorf("select 1 Profile = %q, want cursor", s1.Profile)
	}

	s2, err := p.Select()
	if err != nil {
		t.Fatalf("select 2: %v", err)
	}
	if s2.Profile != "opencode" {
		t.Errorf("select 2 Profile = %q, want opencode", s2.Profile)
	}
}

// TestPoolReset verifies the dashboard's per-key Reset action clears
// consecutive errors, exhaustion, and retirement.
func TestPoolReset(t *testing.T) {
	p := New(Spec{
		Keys:               keys(2),
		Mode:               "round_robin",
		ActiveWindow:       2,
		MaxErrors:          2,
		Cooldown:           5 * time.Hour,
		RetireOnExhaustion: false,
	}, &fakeClock{t: time.Unix(1000, 0)})

	// Exhaust key 1 with two errors.
	r1 := p.RecordError(1)
	if r1.Exhausted {
		t.Fatal("first RecordError should not exhaust yet")
	}
	r2 := p.RecordError(1)
	if !r2.Exhausted {
		t.Fatal("second RecordError should exhaust key 1")
	}
	// Key 1 is exhausted; only key 2 is selectable.
	for i := 0; i < 4; i++ {
		s, err := p.Select()
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if s.Key.Value == "A" {
			t.Fatal("exhausted key A should not be selected")
		}
	}

	// Reset re-enables key 1 immediately, even mid-cooldown.
	p.Reset(1)
	c := p.Counts()
	if c.Exhausted != 0 || c.Active != 2 {
		t.Fatalf("after Reset, counts = %+v, want active=2 exhausted=0", c)
	}

	// Out-of-range Reset is a no-op.
	p.Reset(99)
}
