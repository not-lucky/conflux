package proxy

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time      { return f.t }
func (f *fakeClock) Add(d time.Duration) { f.t = f.t.Add(d) }

func pool(urls ...string) *PoolConfig {
	return &PoolConfig{URLs: urls, MaxErrors: 3, Cooldown: 30 * time.Second}
}

// TestRotationGoldenTable reproduces the rotation example: an active window
// [A,B], a healthy pool [P1,P2,P3], and rotate_interval=2. The test passes
// slotIndex directly: slot0 is A and slot1 is B.
func TestRotationGoldenTable(t *testing.T) {
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	gp := &PoolConfig{URLs: []string{"P1", "P2", "P3"}, RotateInterval: 2, MaxErrors: 3, Cooldown: 30 * time.Second}

	cases := []struct {
		slot, cycle int
		wantURL     string
		wantNum     int
	}{
		{0, 0, "P1", 1},
		{1, 0, "P2", 2},
		{0, 1, "P1", 1}, // wrap to cycle 1, shift 0
		{1, 1, "P2", 2},
		{0, 2, "P2", 2}, // cycle 2, shift 1
		{1, 2, "P3", 3},
	}
	for i, c := range cases {
		r := Resolver{Health: h, GlobalPool: gp, CycleCount: c.cycle}
		s := r.Resolve(c.slot)
		if s.URL != c.wantURL || s.Number != c.wantNum {
			t.Errorf("case %d: slot=%d cycle=%d -> %q num=%d, want %q num=%d",
				i, c.slot, c.cycle, s.URL, s.Number, c.wantURL, c.wantNum)
		}
	}
}

func TestResolveInlineWins(t *testing.T) {
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	r := Resolver{Health: h, Inline: "http://inline:8080", GlobalPool: pool("http://g:8080")}
	s := r.Resolve(0)
	if s.URL != "http://inline:8080" || s.Number != 1 {
		t.Errorf("inline = %+v, want http://inline:8080 num 1", s)
	}
}

func TestInlineTrippedFallsDirect(t *testing.T) {
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	// Trip the inline proxy.
	for i := 0; i < 3; i++ {
		h.RecordError("http://inline:8080", 3, 30*time.Second)
	}
	r := Resolver{Health: h, Inline: "http://inline:8080", GlobalPool: pool("http://g:8080")}
	s := r.Resolve(0)
	if !s.Direct {
		t.Errorf("tripped inline should fall to direct, got %+v", s)
	}
}

func TestProviderPoolOverridesGlobal(t *testing.T) {
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	pp := pool("http://pp:8080")
	gp := pool("http://g:8080")
	r := Resolver{Health: h, ProviderPool: pp, GlobalPool: gp}
	s := r.Resolve(0)
	if s.URL != "http://pp:8080" {
		t.Errorf("provider pool should win, got %q", s.URL)
	}
}

func TestAllProviderTrippedFallsToGlobal(t *testing.T) {
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	for i := 0; i < 3; i++ {
		h.RecordError("http://pp:8080", 3, 30*time.Second)
	}
	pp := pool("http://pp:8080")
	gp := pool("http://g:8080")
	r := Resolver{Health: h, ProviderPool: pp, GlobalPool: gp}
	s := r.Resolve(0)
	if s.URL != "http://g:8080" {
		t.Errorf("all-tripped provider pool should fall to global, got %+v", s)
	}
}

func TestAllTrippedFallsDirect(t *testing.T) {
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	for _, u := range []string{"http://g:8080", "http://g2:8080"} {
		for i := 0; i < 3; i++ {
			h.RecordError(u, 3, 30*time.Second)
		}
	}
	gp := pool("http://g:8080", "http://g2:8080")
	r := Resolver{Health: h, GlobalPool: gp}
	s := r.Resolve(0)
	if !s.Direct {
		t.Errorf("all-tripped global should go direct, got %+v", s)
	}
}

func TestCircuitBreakerRecover(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	h := NewHealth(clk)
	for i := 0; i < 3; i++ {
		h.RecordError("http://g:8080", 3, 30*time.Second)
	}
	if h.Healthy("http://g:8080") {
		t.Error("expected tripped")
	}
	clk.Add(31 * time.Second)
	if !h.Healthy("http://g:8080") {
		t.Error("expected recovered after cooldown")
	}
}

func TestRecordSuccessResets(t *testing.T) {
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	h.RecordError("http://g:8080", 3, 30*time.Second)
	h.RecordError("http://g:8080", 3, 30*time.Second)
	h.RecordSuccess("http://g:8080")
	if !h.Healthy("http://g:8080") {
		t.Error("RecordSuccess should reset breaker")
	}
}

// TestSetLastErrorExposedBySnapshot verifies SetLastError stores the
// transport message and Snapshot surfaces it in LastError, so /_status can
// report the real last error per proxy URL.
func TestSetLastErrorExposedBySnapshot(t *testing.T) {
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	h.SetLastError("http://p:8080", "dial tcp: connection refused")
	entries := h.Snapshot([]string{"http://p:8080"})
	if len(entries) != 1 {
		t.Fatalf("snapshot = %+v, want 1 entry", entries)
	}
	if entries[0].LastError != "dial tcp: connection refused" {
		t.Errorf("LastError = %q, want the transport message", entries[0].LastError)
	}
	if !entries[0].Healthy {
		t.Error("proxy with only a LastError and no errors should be healthy")
	}
	// RecordSuccess clears the last error.
	h.RecordSuccess("http://p:8080")
	entries = h.Snapshot([]string{"http://p:8080"})
	if len(entries) != 1 || entries[0].LastError != "" {
		t.Errorf("after RecordSuccess, LastError = %q, want empty", entries[0].LastError)
	}
}

func TestGlobalPerURL(t *testing.T) {
	// Tripping a URL via one pool excludes it globally for another pool.
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	for i := 0; i < 3; i++ {
		h.RecordError("http://shared:8080", 3, 30*time.Second)
	}
	// A different pool listing the same URL should not select it.
	gp := pool("http://shared:8080", "http://other:8080")
	r := Resolver{Health: h, GlobalPool: gp}
	s := r.Resolve(0)
	if s.URL != "http://other:8080" {
		t.Errorf("expected http://other:8080 (shared tripped globally), got %q", s.URL)
	}
}

func TestRestoreDeadUntil(t *testing.T) {
	clk := &fakeClock{t: time.Unix(100, 0)}
	h := NewHealth(clk)
	du := time.Unix(200, 0)
	h.Restore([]SnapshotEntry{
		{URL: "http://p:8080", ConsecutiveErrors: 2, DeadUntil: du, Healthy: false},
	})
	if h.Healthy("http://p:8080") {
		t.Error("restored dead-until should keep proxy tripped")
	}
	entries := h.Snapshot([]string{"http://p:8080"})
	if len(entries) != 1 || entries[0].ConsecutiveErrors != 2 {
		t.Errorf("snapshot = %+v, want ConsecutiveErrors=2", entries)
	}
	if !entries[0].DeadUntil.Equal(du) {
		t.Errorf("deadUntil = %v, want %v", entries[0].DeadUntil, du)
	}
	// Advance past the dead-until: proxy should recover.
	clk.Add(101 * time.Second)
	if !h.Healthy("http://p:8080") {
		t.Error("proxy should recover after dead-until passes")
	}
}

func TestRestoreSkipsZeroDeadUntil(t *testing.T) {
	h := NewHealth(&fakeClock{t: time.Unix(0, 0)})
	h.Restore([]SnapshotEntry{
		{URL: "http://p:8080", ConsecutiveErrors: 1, DeadUntil: time.Time{}, Healthy: true},
		{URL: "", DeadUntil: time.Unix(50, 0)}, // empty URL skipped
	})
	entries := h.Snapshot([]string{"http://p:8080"})
	if len(entries) != 1 || !entries[0].Healthy {
		t.Errorf("snapshot = %+v, want healthy with no dead-until", entries)
	}
	if entries[0].ConsecutiveErrors != 1 {
		t.Errorf("ConsecutiveErrors = %d, want 1", entries[0].ConsecutiveErrors)
	}
}

func TestHealthTripAndReset(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	h := NewHealth(clk)

	// A healthy URL reports healthy and is unaffected by Reset.
	h.Reset("http://p:8080")
	if !h.Healthy("http://p:8080") {
		t.Fatal("Reset on an untripped URL should keep it healthy")
	}

	// Trip with a cooldown makes it unhealthy until the cooldown elapses.
	h.Trip("http://p:8080", "dial fail", 30*time.Second)
	if h.Healthy("http://p:8080") {
		t.Fatal("Tripped URL should be unhealthy")
	}
	snap := h.Snapshot([]string{"http://p:8080"})
	if snap[0].Healthy || snap[0].LastError != "dial fail" {
		t.Fatalf("snapshot = %+v", snap[0])
	}
	clk.Add(31 * time.Second)
	if !h.Healthy("http://p:8080") {
		t.Fatal("URL should be healthy after cooldown elapses")
	}

	// Trip with zero cooldown trips indefinitely until Reset.
	h.Trip("http://p:8080", "", 0)
	if h.Healthy("http://p:8080") {
		t.Fatal("Tripped URL with zero cooldown should stay unhealthy")
	}
	clk.Add(1000 * time.Hour)
	if h.Healthy("http://p:8080") {
		t.Fatal("zero-cooldown trip should remain tripped far into the future")
	}
	// Reset re-enables it.
	h.Reset("http://p:8080")
	if !h.Healthy("http://p:8080") {
		t.Fatal("Reset should re-enable a tripped URL")
	}
}
