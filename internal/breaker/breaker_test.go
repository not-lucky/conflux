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
