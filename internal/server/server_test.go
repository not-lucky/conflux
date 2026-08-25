package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/not-lucky/conflux/internal/clock"
	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/forward"
	"github.com/not-lucky/conflux/internal/metrics"
	"github.com/not-lucky/conflux/internal/model"
	"github.com/not-lucky/conflux/internal/ratelimit"
	"github.com/not-lucky/conflux/internal/trace"
)

// fakeDoer returns a scripted JSON response.
type fakeDoer struct {
	status int
	body   []byte
	calls  int
}

func (d *fakeDoer) Do(ctx context.Context, req *forward.UpstreamRequest) (*forward.UpstreamResponse, error) {
	d.calls++
	return &forward.UpstreamResponse{
		Status:  d.status,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		BodyBuf: d.body,
		Body:    io.NopCloser(bytes.NewReader(d.body)),
	}, nil
}

// fakeLookup returns a single provider handle with an in-process key and a
// passthrough proxy, which is direct.
type fakeLookup struct{}

func (fakeLookup) Lookup(name string) (forward.ProviderHandle, error) {
	return &fakeServerHandle{name: name}, nil
}

// fakeServerHandle implements forward.ProviderHandle for server tests.
type fakeServerHandle struct {
	name string
}

func (h *fakeServerHandle) Policy() forward.ProviderPolicy {
	return forward.ProviderPolicy{
		Name:              h.name,
		BaseURL:           "https://upstream.example.com/v1",
		MaxAttempts:       1,
		ActiveWindowSize:  1,
		MaxStreamRetries:  0,
		RequestTimeout:    30 * time.Second,
		StreamIdleTimeout: 15 * time.Second,
		KeepaliveInterval: 15 * time.Second,
		RequestDeadline:   60 * time.Second,
	}
}

func (h *fakeServerHandle) Select() (forward.Selection, error) {
	return forward.Selection{Key: "sk-test", KeyNumber: 1, SlotIndex: 0}, nil
}
func (h *fakeServerHandle) RecordSuccess(keyNumber int) {}
func (h *fakeServerHandle) RecordError(keyNumber int) forward.RecordResult {
	return forward.RecordResult{}
}
func (h *fakeServerHandle) MarkExhausted(keyNumber int) {}
func (h *fakeServerHandle) ResolveProxy(slotIndex, cycleCount int, sel forward.Selection) forward.ProxySelection {
	return forward.ProxySelection{Direct: true}
}
func (h *fakeServerHandle) RecordProxyError(url string)       {}
func (h *fakeServerHandle) SetProxyLastError(url, msg string) {}
func (h *fakeServerHandle) RecordProxySuccess(url string)     {}
func (h *fakeServerHandle) BreakerOpen() bool                 { return false }
func (h *fakeServerHandle) BreakerOn5xx() bool                { return false }
func (h *fakeServerHandle) BreakerOn2xx()                     {}

func newTestServerWithConfig(t *testing.T, doer forward.Doer) *Server {
	t.Helper()
	reg := model.NewRegistry([]model.Provider{
		{Name: "openai", Models: []model.Entry{{Kind: model.Exact, Literal: "gpt-4o"}}},
	})
	cfg, err := config.Parse([]byte(testYAML))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	fwd := &forward.Forwarder{Doer: doer, Providers: fakeLookup{}}
	mreg := metrics.New(time.Unix(0, 0))
	lim := ratelimit.New(clock.RealClock{})
	tr := trace.New(t.TempDir(), trace.Off, 10)
	return New(cfg, reg, fwd, lim, mreg, tr, nil)
}

const testYAML = `
server:
  port: 24118
  request_timeout: 30s
  stream_idle_timeout: 15s
  stream_keepalive_interval: 15s
  request_deadline: 60s
auth:
  client_keys:
    - "sk-client"
logging:
  level: off
  max_dirs: 10
defaults:
  max_errors: 5
  cooldown: 1h
  max_stream_retries: 0
  upstream_5xx_threshold: 5
  upstream_5xx_cooldown: 30s
  request_timeout: 30s
  stream_idle_timeout: 15s
  stream_keepalive_interval: 15s
  request_deadline: 60s
providers:
  openai:
    base_url: "https://upstream.example.com/v1"
    keys:
      - key: "sk-test"
    models:
      - gpt-4o
`

func TestServerProxyJSON(t *testing.T) {
	doer := &fakeDoer{status: 200, body: []byte(`{"id":"chatcmpl-1","object":"chat.completion"}`)}
	srv := newTestServerWithConfig(t, doer)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "chatcmpl-1") {
		t.Errorf("body = %s", b)
	}
	// x-conflux-* diagnostics.
	if got := resp.Header.Get("X-Conflux-Provider"); got != "openai" {
		t.Errorf("X-Conflux-Provider = %q", got)
	}
	if got := resp.Header.Get("X-Conflux-Key"); got != "1" {
		t.Errorf("X-Conflux-Key = %q, want 1", got)
	}
	if doer.calls != 1 {
		t.Errorf("doer.calls = %d, want 1", doer.calls)
	}
}

func TestServerUnauthorized(t *testing.T) {
	doer := &fakeDoer{status: 200, body: []byte(`{}`)}
	srv := newTestServerWithConfig(t, doer)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-4o"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestServerModelNotFound(t *testing.T) {
	doer := &fakeDoer{status: 200, body: []byte(`{}`)}
	srv := newTestServerWithConfig(t, doer)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"no-such-model"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := ts.Client().Do(req)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestServerModelsList(t *testing.T) {
	doer := &fakeDoer{status: 200, body: []byte(`{}`)}
	srv := newTestServerWithConfig(t, doer)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "gpt-4o") {
		t.Errorf("models list = %s", b)
	}
}

func TestServerStatus(t *testing.T) {
	doer := &fakeDoer{status: 200, body: []byte(`{}`)}
	srv := newTestServerWithConfig(t, doer)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/_status")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"ok": true`) {
		t.Errorf("_status missing ok:true:\n%s", b)
	}
	if !strings.Contains(string(b), "sk…nt") {
		t.Errorf("_status should mask client keys: %s", b)
	}
}

func TestServerTerminalError(t *testing.T) {
	// A doer that always returns a transport error simulates an unreachable
	// upstream; with MaxAttempts=1 the forwarder returns Status==0 with an
	// error.
	doer := &errDoer{}
	srv := newTestServerWithConfig(t, doer)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-4o"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := ts.Client().Do(req)
	if resp.StatusCode != 502 {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// errDoer always fails the fetch with a transport error.
type errDoer struct{}

func (errDoer) Do(ctx context.Context, req *forward.UpstreamRequest) (*forward.UpstreamResponse, error) {
	return nil, errors.New("dial tcp: connection refused")
}

func TestServerMetrics(t *testing.T) {
	doer := &fakeDoer{status: 200, body: []byte(`{}`)}
	srv := newTestServerWithConfig(t, doer)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Drive one proxied request.
	body := `{"model":"gpt-4o"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	req.Header.Set("Content-Type", "application/json")
	_, _ = ts.Client().Do(req)

	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "conflux_requests_total") {
		t.Errorf("metrics missing requests_total:\n%s", b)
	}
}

// newTestServerWithTracer builds a Server whose Tracer is caller-supplied, so
// a test can drive a real handleProxy request at level Full and assert trace
// files on disk.
func newTestServerWithTracer(t *testing.T, doer forward.Doer, tr *trace.Tracer) *Server {
	t.Helper()
	reg := model.NewRegistry([]model.Provider{
		{Name: "openai", Models: []model.Entry{{Kind: model.Exact, Literal: "gpt-4o"}}},
	})
	cfg, err := config.Parse([]byte(testYAML))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	fwd := &forward.Forwarder{Doer: doer, Providers: fakeLookup{}}
	mreg := metrics.New(time.Unix(0, 0))
	lim := ratelimit.New(clock.RealClock{})
	return New(cfg, reg, fwd, lim, mreg, tr, nil)
}

// TestServerTraceFull drives a real JSON request through handleProxy with the
// Tracer at level Full and asserts that request.json, response.json, and
// meta.json are written under the trace dir.
func TestServerTraceFull(t *testing.T) {
	root := t.TempDir()
	tr := trace.New(root, trace.Full, 1000)
	doer := &fakeDoer{status: 200, body: []byte(`{"id":"chatcmpl-1","object":"chat.completion"}`)}
	srv := newTestServerWithTracer(t, doer, tr)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions?api_key=secret", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// There must be exactly one trace dir containing the expected files.
	traceDir := filepath.Join(root, "trace")
	reqFiles, err := filepath.Glob(filepath.Join(traceDir, "*", "request.json"))
	if err != nil {
		t.Fatalf("glob request.json: %v", err)
	}
	if len(reqFiles) != 1 {
		t.Fatalf("request.json files = %d, want 1", len(reqFiles))
	}
	traceSub := filepath.Dir(reqFiles[0])
	for _, want := range []string{"request.json", "response_headers.json", "response.json", "meta.json"} {
		if _, err := os.Stat(filepath.Join(traceSub, want)); err != nil {
			t.Errorf("missing %q in trace dir: %v", want, err)
		}
	}

	// request.json must redact the Authorization header and the api_key query.
	reqBytes, _ := os.ReadFile(reqFiles[0])
	reqStr := string(reqBytes)
	if strings.Contains(reqStr, "sk-client") {
		t.Error("Authorization not redacted in request.json")
	}
	if strings.Contains(reqStr, "api_key=secret") {
		t.Error("api_key not redacted in request.json URL")
	}
	if !strings.Contains(reqStr, "api_key=****") {
		t.Error("expected api_key=**** in request.json URL")
	}

	// response.json must contain the upstream body.
	respBytes, _ := os.ReadFile(filepath.Join(traceSub, "response.json"))
	if !strings.Contains(string(respBytes), "chatcmpl-1") {
		t.Errorf("response.json = %s, want chatcmpl-1", respBytes)
	}

	// meta.json must record the provider and model.
	metaBytes, _ := os.ReadFile(filepath.Join(traceSub, "meta.json"))
	if !strings.Contains(string(metaBytes), "openai") || !strings.Contains(string(metaBytes), "gpt-4o") {
		t.Errorf("meta.json = %s, want provider openai + model gpt-4o", metaBytes)
	}
}

// TestServerTraceOff asserts that at level Off no trace or error dirs are
// created, even for a terminal-failure request.
func TestServerTraceOff(t *testing.T) {
	root := t.TempDir()
	tr := trace.New(root, trace.Off, 1000)
	doer := &errDoer{}
	srv := newTestServerWithTracer(t, doer, tr)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-4o"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := ts.Client().Do(req)
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	for _, sub := range []string{"trace", "error"} {
		if _, err := os.Stat(filepath.Join(root, sub)); !os.IsNotExist(err) {
			t.Errorf("%s dir should not exist at level Off", sub)
		}
	}
}

// TestServerTraceErrorsOnly asserts that a terminal failure at level
// ErrorsOnly writes an error trace but no per-request trace dir.
func TestServerTraceErrorsOnly(t *testing.T) {
	root := t.TempDir()
	tr := trace.New(root, trace.ErrorsOnly, 1000)
	doer := &errDoer{}
	srv := newTestServerWithTracer(t, doer, tr)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-4o"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-client")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := ts.Client().Do(req)
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	// No per-request trace dir at ErrorsOnly.
	if _, err := os.Stat(filepath.Join(root, "trace")); !os.IsNotExist(err) {
		t.Errorf("trace dir should not exist at ErrorsOnly")
	}
	// An error trace must exist.
	errFiles, err := filepath.Glob(filepath.Join(root, "error", "*", "error.json"))
	if err != nil {
		t.Fatalf("glob error.json: %v", err)
	}
	if len(errFiles) != 1 {
		t.Fatalf("error.json files = %d, want 1", len(errFiles))
	}
	errBytes, _ := os.ReadFile(errFiles[0])
	if !strings.Contains(string(errBytes), "PROXY_NETWORK_ERROR") {
		t.Errorf("error.json = %s, want PROXY_NETWORK_ERROR", errBytes)
	}
}

// TestServerStatusRealProxyHealth verifies that /_status reports the real
// proxy health and LastError from the app-supplied proxyHealth closure, not
// the always-true placeholder. Gauge mutation is now handled by the app on
// state change, not by /_status, so this test no longer asserts gauge values
// in /metrics.
func TestServerStatusRealProxyHealth(t *testing.T) {
	doer := &fakeDoer{status: 200, body: []byte(`{}`)}

	reg := model.NewRegistry([]model.Provider{
		{Name: "openai", Models: []model.Entry{{Kind: model.Exact, Literal: "gpt-4o"}}},
	})
	cfg, err := config.Parse([]byte(testYAML))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.Proxies = config.GlobalProxyConfig{
		URLs:      []string{"http://tripped:8080", "http://ok:8080"},
		MaxErrors: 3, Cooldown: 30 * time.Second,
	}

	fwd := &forward.Forwarder{Doer: doer, Providers: fakeLookup{}}
	mreg := metrics.New(time.Unix(0, 0))
	lim := ratelimit.New(clock.RealClock{})
	tr := trace.New(t.TempDir(), trace.Off, 10)

	// proxyHealth closure: http://tripped:8080 is unhealthy with a LastError.
	deadMs := int64(1700000000000)
	proxyHealth := func() map[string]metrics.ProxyHealth {
		return map[string]metrics.ProxyHealth{
			"http://tripped:8080": {
				Healthy:           false,
				ConsecutiveErrors: 3,
				DeadUntil:         &deadMs,
				LastError:         "dial tcp: connection refused",
			},
			"http://ok:8080": {Healthy: true},
		}
	}

	srv := New(cfg, reg, fwd, lim, mreg, tr, proxyHealth)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// /_status must report the real health.
	resp, err := ts.Client().Get(ts.URL + "/_status")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, `"healthy": false`) {
		t.Errorf("_status should report tripped proxy as healthy=false:\n%s", body)
	}
	if !strings.Contains(body, "dial tcp: connection refused") {
		t.Errorf("_status should surface LastError:\n%s", body)
	}
	if !strings.Contains(body, `"consecutiveErrors": 3`) {
		t.Errorf("_status should surface ConsecutiveErrors:\n%s", body)
	}
}
