package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

// TestDoKeepsContextAliveWhileReadingBody is a regression test for the bug
// where the doer canceled the TTFB context immediately after the response
// headers arrived — before the response body was read. Because the request
// (and thus the response body) reads through that context, canceling it
// mid-response RST_STREAMs the HTTP/2 stream: the server observes its request
// context cancel before it has finished writing the body, and the pooled
// client connection is torn down so later reuses of the cached transport fail
// with "context canceled" (surfaced upstream as PROXY_NETWORK_ERROR).
//
// The fix reads and closes the body while the TTFB context is still live, then
// cancels. The test asserts the server's request context stays alive
// throughout the body write, proving the client no longer cancels mid-body.
func TestDoKeepsContextAliveWhileReadingBody(t *testing.T) {
	var canceledDuringBody atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Flush headers so the client receives them (and, with the bug, cancels
		// its TTFB context) before the body is written.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(150 * time.Millisecond)
		// If the buggy client canceled its TTFB context right after the headers,
		// the RST_STREAM has propagated and canceled the server-side request
		// context by now. The fixed client keeps the context alive until the
		// body is fully read, so it is still nil here.
		if r.Context().Err() != nil {
			canceledDuringBody.Store(true)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	rt := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: true,
	}
	d := newHTTPDoer()
	d.transports[""] = rt

	resp, err := d.Do(context.Background(), &forward.UpstreamRequest{
		Method:  "POST",
		URL:     srv.URL,
		TTFB:    5 * time.Second,
		Body:    []byte(`{}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
	if err != nil {
		t.Fatalf("Do: unexpected error: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if !bytes.Equal(resp.BodyBuf, []byte(`{"ok":true}`)) {
		t.Fatalf("body = %q, want {\"ok\":true}", string(resp.BodyBuf))
	}
	if canceledDuringBody.Load() {
		t.Error("server request context was canceled before the body was fully written; the doer must keep the request context alive until the body is read")
	}
}
