package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/not-lucky/conflux/internal/forward"
)

// TestTransportForCacheHit asserts transportFor returns the same RoundTripper
// instance for the same viaProxy (cache hit) and a different instance for
// different viaProxy values.
func TestTransportForCacheHit(t *testing.T) {
	d := newHTTPDoer()

	t1, err := d.transportFor("http://p:8080")
	if err != nil {
		t.Fatalf("transportFor(http://p:8080) #1: %v", err)
	}
	t2, err := d.transportFor("http://p:8080")
	if err != nil {
		t.Fatalf("transportFor(http://p:8080) #2: %v", err)
	}
	if t1 != t2 {
		t.Error("transport not cached: same viaProxy returned different instances")
	}

	t3, err := d.transportFor("")
	if err != nil {
		t.Fatalf("transportFor(\"\"): %v", err)
	}
	if t1 == t3 {
		t.Error("different proxies shared a transport")
	}

	// A different proxy URL must also get its own transport.
	t4, err := d.transportFor("http://q:9090")
	if err != nil {
		t.Fatalf("transportFor(http://q:9090): %v", err)
	}
	if t1 == t4 {
		t.Error("distinct proxy URLs shared a transport")
	}
	if t3 == t4 {
		t.Error("empty proxy and http://q:9090 shared a transport")
	}
}

// TestDoTTFBCancel asserts that on a TTFB timeout the doer cancels its own
// in-flight request context promptly (not at the forwarder's deadline) and
// returns the TTFB error. The test server's handler sleeps longer than the
// TTFB and records whether its request context was cancelled within a short
// window, proving the doer cancels at TTFB.
func TestDoTTFBCancel(t *testing.T) {
	var cancelled atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wait for the request context to be cancelled. If the doer cancels at
		// TTFB, this fires well before the 2s grace window; if it only cancels
		// at a long forwarder deadline, the test's own timeout fails first.
		select {
		case <-r.Context().Done():
			cancelled.Store(true)
		case <-time.After(2 * time.Second):
			// No cancellation observed within the window.
			cancelled.Store(false)
		}
	}))
	defer srv.Close()

	d := newHTTPDoer()
	ttfb := 50 * time.Millisecond

	start := time.Now()
	_, err := d.Do(context.Background(), &forward.UpstreamRequest{
		Method: "GET",
		URL:    srv.URL,
		TTFB:   ttfb,
	})
	elapsed := time.Since(start)

	// The error must be the TTFB sentinel.
	if err == nil {
		t.Fatal("expected TTFB error, got nil")
	}
	if !strings.Contains(err.Error(), "ttfb timeout") {
		t.Fatalf("unexpected error: %v", err)
	}

	// The error must return promptly at TTFB, not linger to a long deadline.
	if elapsed > ttfb+500*time.Millisecond {
		t.Errorf("Do returned too slowly: %v (ttfb=%v)", elapsed, ttfb)
	}

	// The handler must observe its context cancellation. The client cancels
	// ttfbCtx at TTFB, which closes the underlying connection; the server then
	// detects the close and cancels r.Context(). That propagation takes a
	// moment, so poll for up to 2s (the handler's own grace window).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !cancelled.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	if !cancelled.Load() {
		t.Error("server handler did not observe request context cancellation")
	}
}
