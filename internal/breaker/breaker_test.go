package breaker

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time      { return f.t }
func (f *fakeClock) Add(d time.Duration) { f.t = f.t.Add(d) }

func TestBreakerOpenAndClose(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	b := New(3, 30*time.Second, clk)
	for i := 0; i < 2; i++ {
		b.On5xx()
		if b.Open() {
			t.Error("breaker should not open below threshold")
		}
	}
	if !b.On5xx() {
		t.Error("3rd On5xx should open")
	}
	if !b.Open() {
		t.Error("expected open")
	}
	// A 2xx response closes the breaker immediately.
	b.On2xx()
	if b.Open() {
		t.Error("expected closed after 2xx")
	}
}

func TestBreakerHalfOpenReopen(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	b := New(1, 10*time.Second, clk)
	b.On5xx()
	if !b.Open() {
		t.Fatal("expected open")
	}
	clk.Add(11 * time.Second)
	if b.Open() {
		t.Error("after cooldown, half-open should report not-open")
	}
	// A 5xx now reopens.
	b.On5xx()
	if !b.Open() {
		t.Error("expected reopened after half-open 5xx")
	}
}

func TestBreakerForceOpenAndReset(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	b := New(5, 30*time.Second, clk)

	// Reset on a closed breaker is a no-op-ish but must keep it closed.
	b.Reset()
	if b.Open() {
		t.Fatal("Reset on a closed breaker should keep it closed")
	}

	// ForceOpen opens it even with zero 5xx count.
	b.ForceOpen(0)
	if !b.Open() {
		t.Fatal("ForceOpen should open the breaker")
	}
	// After the configured cooldown elapses, it becomes half-open.
	clk.Add(31 * time.Second)
	if b.Open() {
		t.Fatal("breaker should be half-open after configured cooldown")
	}
	// ForceOpen with an explicit cooldown is respected.
	b.ForceOpen(2 * time.Second)
	if !b.Open() {
		t.Fatal("ForceOpen with explicit cooldown should open")
	}
	clk.Add(1 * time.Second)
	if !b.Open() {
		t.Fatal("still within explicit cooldown, should remain open")
	}
	// Reset closes it immediately, even mid-cooldown.
	b.Reset()
	if b.Open() {
		t.Fatal("Reset should close the breaker immediately")
	}
}
