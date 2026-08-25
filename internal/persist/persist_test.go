package persist

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHashKey(t *testing.T) {
	h := HashKey("sk-proj-abc")
	if h == "" || h[:7] != "sha256:" {
		t.Errorf("HashKey = %q, want sha256: prefix", h)
	}
	if HashKey("sk-proj-abc") != h {
		t.Error("HashKey not deterministic")
	}
}

func TestSaveLoadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	s := New(path)
	ex := time.Unix(1700000000, 0).UTC()
	s.Set(State{
		Keys: []KeyRecord{{
			Provider: "openai", KeyHash: HashKey("sk-1"),
			ConsecutiveErrors: 2, ExhaustedAt: &ex,
		}},
		Proxies: []ProxyRecord{{URL: "http://p:8080", ConsecutiveErrors: 1}},
	})
	s.FlushImmediately()

	data, _ := os.ReadFile(path)
	if len(data) == 0 {
		t.Fatal("state file not written")
	}

	s2 := New(path)
	st, err := s2.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Keys) != 1 || st.Keys[0].Provider != "openai" {
		t.Errorf("keys = %+v", st.Keys)
	}
	if st.Keys[0].KeyHash != HashKey("sk-1") {
		t.Errorf("key hash = %q", st.Keys[0].KeyHash)
	}
	if st.Keys[0].ConsecutiveErrors != 2 {
		t.Errorf("consecutiveErrors = %d", st.Keys[0].ConsecutiveErrors)
	}
	if st.Keys[0].ExhaustedAt == nil || !st.Keys[0].ExhaustedAt.Equal(ex) {
		t.Errorf("exhaustedAt = %v", st.Keys[0].ExhaustedAt)
	}
	if len(st.Proxies) != 1 || st.Proxies[0].URL != "http://p:8080" {
		t.Errorf("proxies = %+v", st.Proxies)
	}
}

func TestSaveLoadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := New(path)
	s.Set(State{Keys: []KeyRecord{{Provider: "p", KeyHash: HashKey("k")}}})
	s.FlushImmediately()
	s2 := New(path)
	st, err := s2.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Keys) != 1 {
		t.Errorf("keys = %d", len(st.Keys))
	}
}

func TestLoadMissing(t *testing.T) {
	s := New("/nonexistent/state.yaml")
	st, err := s.Load()
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(st.Keys) != 0 {
		t.Errorf("expected empty state")
	}
}

func TestDisabled(t *testing.T) {
	s := New("")
	s.Set(State{Keys: []KeyRecord{{Provider: "p"}}})
	s.FlushImmediately() // a no-op for a disabled store
	st, err := s.Load()
	if err != nil || len(st.Keys) != 0 {
		t.Errorf("disabled store should be empty no-op")
	}
}

func TestDisabledStoreStopReturns(t *testing.T) {
	s := New("")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.StartFlusher(ctx)
	select {
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung on a disabled store (doneCh never closed)")
	case <-stopDone(s):
	}
}

// stopDone signals when Store.Stop returns. Stop blocks until doneCh is
// closed, so this returns only when StartFlusher has shut down cleanly.
func stopDone(s *Store) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		s.Stop()
		close(ch)
	}()
	return ch
}

func TestFlusherFlushesOnStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	s := New(path)
	s.Set(State{Keys: []KeyRecord{{Provider: "p", KeyHash: HashKey("k")}}})
	ctx, cancel := context.WithCancel(context.Background())
	go s.StartFlusher(ctx)
	time.Sleep(20 * time.Millisecond) // let the flusher reach the select
	// Stop flushes immediately.
	s.Stop()
	cancel()
	data, _ := os.ReadFile(path)
	if len(data) == 0 {
		t.Fatal("flusher should have written on Stop")
	}
}
