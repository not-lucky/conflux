package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"

	"github.com/not-lucky/conflux/internal/forward"
	"github.com/not-lucky/conflux/internal/stream"
	"golang.org/x/net/proxy"
)

// httpDoer is the production Doer adapter. It supports http and socks5
// proxies and buffers JSON response bodies for classification.
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
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bodyReader(req.Body))
	if err != nil {
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
		return nil, err
	}
	c := &http.Client{Transport: transport, Timeout: 0}

	resp, err := c.Do(httpReq)
	if err != nil {
		return nil, err
	}
	return d.readResponse(resp, req)
}

// readResponse finalizes the upstream response. For a buffered (JSON) response
// it reads the body up to a reasonable cap for classification and closes the body.
// For an SSE stream it returns the response body directly.
func (d *httpDoer) readResponse(resp *http.Response, req *forward.UpstreamRequest) (*forward.UpstreamResponse, error) {
	ct := resp.Header.Get("Content-Type")
	if stream.IsSSE(ct) {
		return &forward.UpstreamResponse{
			Status:  resp.StatusCode,
			Headers: resp.Header,
			Body:    resp.Body,
			IsSSE:   true,
		}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	resp.Body.Close()
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
