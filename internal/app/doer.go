package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/not-lucky/conflux/internal/forward"
	"github.com/not-lucky/conflux/internal/stream"
	"golang.org/x/net/proxy"
)

// httpDoer is the production Doer adapter. It supports http and socks5
// proxies, a per-attempt time-to-first-byte (TTFB) deadline enforced through
// the response-header context, and buffers JSON response bodies for
// classification.
//
// Transports are cached by viaProxy so their internal connection pools
// persist across attempts and requests: repeated calls to the same proxy
// reuse the same http.RoundTripper (and thus its pooled connections) instead
// of doing a fresh TLS handshake every time.
type httpDoer struct {
	mu         sync.Mutex
	transports map[string]http.RoundTripper
}

func newHTTPDoer() *httpDoer {
	return &httpDoer{transports: make(map[string]http.RoundTripper)}
}

func (d *httpDoer) Do(ctx context.Context, req *forward.UpstreamRequest) (*forward.UpstreamResponse, error) {
	// Wrap the per-attempt context with a TTFB cancel so that, on a TTFB
	// timeout, the in-flight c.Do is cancelled promptly and its connection is
	// freed — instead of lingering until the forwarder's deadline fires.
	ttfbCtx, ttfbCancel := context.WithCancel(ctx)

	httpReq, err := http.NewRequestWithContext(ttfbCtx, req.Method, req.URL, bodyReader(req.Body))
	if err != nil {
		ttfbCancel()
		return nil, err
	}
	httpReq.Header = req.Headers.Clone()
	if len(req.Body) > 0 {
		httpReq.ContentLength = int64(len(req.Body))
	}

	// Use the cached long-lived transport so its connection pool persists
	// across attempts and requests. The http.Client is a small struct; building
	// one per attempt is cheap and avoids sharing a mutable Transport field
	// across concurrent requests with different proxies.
	transport, err := d.transportFor(req.ViaProxy)
	if err != nil {
		ttfbCancel()
		return nil, err
	}
	c := &http.Client{Transport: transport, Timeout: 0}

	// Enforce TTFB: cancel the request when the headers do not arrive within
	// req.TTFB.
	ttfb := req.TTFB
	if ttfb <= 0 {
		ttfb = 60 * time.Second
	}
	type result struct {
		resp *http.Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := c.Do(httpReq)
		ch <- result{resp, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			// The fetch failed before headers (dial, TLS, or a transport
			// error). Cancel ttfbCtx now; there is no body to drain.
			ttfbCancel()
			return nil, r.err
		}
		// Headers arrived: the TTFB deadline is satisfied. ttfbCtx is still the
		// context the response body reads through, so it MUST stay alive until
		// the body is fully consumed. Canceling it here — as this code did
		// before — tears down the pooled HTTP/2 connection mid-read, so the
		// next attempt that reuses this cached transport fails instantly with
		// "context canceled" (surfaced as PROXY_NETWORK_ERROR). For a buffered
		// JSON response we read+close the body now and then cancel; for an SSE
		// stream we hand the body off with a cancel-on-close wrapper so the
		// stream stays live until the downstream consumer closes it.
		return d.readResponse(r.resp, req, ttfbCancel)
	case <-time.After(ttfb):
		// Cancel the in-flight request at TTFB (not at the forwarder's
		// deadline) so c.Do returns promptly and the upstream connection is
		// freed. A drain goroutine still closes any response body that arrives
		// after the cancel.
		ttfbCancel()
		go func() {
			if r := <-ch; r.resp != nil {
				r.resp.Body.Close()
			}
		}()
		return nil, errors.New("ttfb timeout: upstream headers deadline exceeded")
	}
}

// readResponse finalizes the upstream response. For a buffered (JSON) response
// it reads the body up to a reasonable cap for classification, closes the body,
// and then cancels the TTFB context — in that order, so the body read happens
// under a live request context and the pooled connection is not torn down
// mid-read. For an SSE stream it hands the body off wrapped in a
// cancel-on-close reader, so the stream stays live for its full lifetime and
// the TTFB context is canceled when the downstream consumer closes the body.
// ttfbCancel is always invoked exactly once.
func (d *httpDoer) readResponse(resp *http.Response, req *forward.UpstreamRequest, ttfbCancel context.CancelFunc) (*forward.UpstreamResponse, error) {
	ct := resp.Header.Get("Content-Type")
	if stream.IsSSE(ct) {
		// For a stream, return the body as a ReadCloser for the forwarder to
		// peek. Keep ttfbCtx alive for the stream's lifetime; cancel it when the
		// body is closed so the underlying connection is released promptly once
		// the stream is done.
		return &forward.UpstreamResponse{
			Status:  resp.StatusCode,
			Headers: resp.Header,
			Body:    &cancelOnClose{ReadCloser: resp.Body, cancel: ttfbCancel},
			IsSSE:   true,
		}, nil
	}
	// For JSON and other buffered responses, read the body up to a reasonable
	// cap for classification. Read and close while ttfbCtx is still live so the
	// body read is not aborted mid-flight; only then cancel the TTFB context.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	resp.Body.Close()
	ttfbCancel()
	if err != nil {
		return nil, err
	}
	return &forward.UpstreamResponse{
		Status:  resp.StatusCode,
		Headers: resp.Header,
		Body:    io.NopCloser(bytes.NewReader(body)),
		BodyBuf: body,
	}, nil
}

func (d *httpDoer) transportFor(viaProxy string) (http.RoundTripper, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rt, ok := d.transports[viaProxy]; ok {
		return rt, nil
	}
	rt, err := d.buildTransport(viaProxy)
	if err != nil {
		return nil, err
	}
	d.transports[viaProxy] = rt
	return rt, nil
}

func (d *httpDoer) buildTransport(viaProxy string) (http.RoundTripper, error) {
	base := &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: false},
		ForceAttemptHTTP2: true,
	}
	if viaProxy == "" {
		return base, nil
	}
	u, err := url.Parse(viaProxy)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http", "https":
		base.Proxy = http.ProxyURL(u)
		return base, nil
	case "socks5", "socks5h":
		dialer, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, err
		}
		base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
		return base, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

func bodyReader(b []byte) io.Reader {
	if len(b) == 0 {
		return nil
	}
	return bytes.NewReader(b)
}

// cancelOnClose wraps an io.ReadCloser so that Close invokes a cancel func
// exactly once, in addition to closing the wrapped body. It is used to tie the
// TTFB request context's lifetime to an SSE stream body: the stream reads
// through that context for its full lifetime, and the context is canceled
// (freeing the pooled connection) only once the downstream consumer closes
// the body.
type cancelOnClose struct {
	once   sync.Once
	cancel context.CancelFunc
	io.ReadCloser
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}
