package forward

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeDoer scripts a sequence of responses and transport-errors per attempt.
type fakeDoer struct {
	script []fakeResp
	calls  []*UpstreamRequest
	idx    int
}

type fakeResp struct {
	status       int
	headers      http.Header
	body         []byte
	isSSE        bool
	sseChunk     []byte // first-chunk bytes for SSE responses; Body is set to this
	transportErr string
}

func (d *fakeDoer) Do(ctx context.Context, req *UpstreamRequest) (*UpstreamResponse, error) {
	d.calls = append(d.calls, req)
	if d.idx >= len(d.script) {
		return nil, errors.New("no more scripted responses")
	}
	r := d.script[d.idx]
	d.idx++
	if r.transportErr != "" {
		return &UpstreamResponse{TransportErr: r.transportErr}, errors.New(r.transportErr)
	}
	h := r.headers
	if h == nil {
		h = http.Header{}
	}
	body := r.body
	if r.isSSE {
		body = r.sseChunk
	}
	bodyReader := io.NopCloser(bytes.NewReader(body))
	return &UpstreamResponse{Status: r.status, Headers: h, Body: bodyReader, BodyBuf: r.body, IsSSE: r.isSSE}, nil
}

// fakeProvider implements ProviderLookup for tests. It holds a single
// *fakeHandle that implements ProviderHandle.
type fakeProvider struct {
	handle *fakeHandle
}

func (fp *fakeProvider) Lookup(name string) (ProviderHandle, error) {
	if name != fp.handle.name {
		return nil, errors.New("unknown provider")
	}
	return fp.handle, nil
}

func newFakeProvider(name string) (*fakeProvider, *fakeHandle) {
	fh := &fakeHandle{
		name: name,
		policy: ProviderPolicy{
			Name:              name,
			BaseURL:           "https://api.example.com/v1",
			MaxAttempts:       3,
			ActiveWindowSize:  3,
			MaxStreamRetries:  3,
			RequestTimeout:    5 * time.Second,
			StreamIdleTimeout: 5 * time.Second,
			KeepaliveInterval: 0,
			RequestDeadline:   30 * time.Second,
		},
	}
	fh.ResetDefaults()
	return &fakeProvider{handle: fh}, fh
}

// fakeHandle implements ProviderHandle for tests. Its exported func fields are
// the method implementations, so tests can swap individual behaviors (e.g.
// fh.BreakerOpenFn = func() bool { return true }) without rebuilding the
// handle.
type fakeHandle struct {
	name    string
	nextKey int
	policy  ProviderPolicy

	SelectFn             func() (Selection, error)
	RecordSuccessFn      func(keyNumber int)
	RecordErrorFn        func(keyNumber int) RecordResult
	MarkExhaustedFn      func(keyNumber int)
	ResolveProxyFn       func(slotIndex, cycleCount int, sel Selection) ProxySelection
	RecordProxyErrorFn   func(url string)
	SetProxyLastErrorFn  func(url, msg string)
	RecordProxySuccessFn func(url string)
	BreakerOpenFn        func() bool
	BreakerOn5xxFn       func() bool
	BreakerOn2xxFn       func()
}

// ResetDefaults installs the no-op/default behavior for every func field,
// so a test can override just the ones it cares about.
func (h *fakeHandle) ResetDefaults() {
	h.SelectFn = h.selectKey
	h.RecordSuccessFn = func(int) {}
	h.RecordErrorFn = func(int) RecordResult { return RecordResult{} }
	h.MarkExhaustedFn = func(int) {}
	h.ResolveProxyFn = func(slot, cycle int, sel Selection) ProxySelection { return ProxySelection{Direct: true} }
	h.RecordProxyErrorFn = func(string) {}
	h.SetProxyLastErrorFn = func(string, string) {}
	h.RecordProxySuccessFn = func(string) {}
	h.BreakerOpenFn = func() bool { return false }
	h.BreakerOn5xxFn = func() bool { return false }
	h.BreakerOn2xxFn = func() {}
}

func (h *fakeHandle) Policy() ProviderPolicy { return h.policy }

func (h *fakeHandle) Select() (Selection, error)             { return h.SelectFn() }
func (h *fakeHandle) RecordSuccess(keyNumber int)            { h.RecordSuccessFn(keyNumber) }
func (h *fakeHandle) RecordError(keyNumber int) RecordResult { return h.RecordErrorFn(keyNumber) }
func (h *fakeHandle) MarkExhausted(keyNumber int)            { h.MarkExhaustedFn(keyNumber) }
func (h *fakeHandle) ResolveProxy(slotIndex, cycleCount int, sel Selection) ProxySelection {
	return h.ResolveProxyFn(slotIndex, cycleCount, sel)
}
func (h *fakeHandle) RecordProxyError(url string)       { h.RecordProxyErrorFn(url) }
func (h *fakeHandle) SetProxyLastError(url, msg string) { h.SetProxyLastErrorFn(url, msg) }
func (h *fakeHandle) RecordProxySuccess(url string)     { h.RecordProxySuccessFn(url) }
func (h *fakeHandle) BreakerOpen() bool                 { return h.BreakerOpenFn() }
func (h *fakeHandle) BreakerOn5xx() bool                { return h.BreakerOn5xxFn() }
func (h *fakeHandle) BreakerOn2xx()                     { h.BreakerOn2xxFn() }

func (h *fakeHandle) selectKey() (Selection, error) {
	h.nextKey++
	return Selection{Key: "sk-provider-key-" + strconv.Itoa(h.nextKey), KeyNumber: h.nextKey, SlotIndex: 0}, nil
}

func TestForwardSuccess(t *testing.T) {
	doer := &fakeDoer{script: []fakeResp{{status: 200, body: []byte(`{"ok":true}`)}}}
	fp, _ := newFakeProvider("openai")
	f := &Forwarder{Doer: doer, Providers: fp}
	req := &Request{Method: "POST", Path: "/chat/completions", Body: []byte(`{"model":"gpt-4o"}`), Model: "gpt-4o", Provider: "openai", Headers: http.Header{}}
	resp, err := f.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d", resp.Status)
	}
	if resp.Provider != "openai" {
		t.Errorf("provider = %q", resp.Provider)
	}
	// x-conflux-* diagnostics are carried on the Response struct fields; the
	// server renders them into headers. forward no longer injects into
	// resp.Headers.
	if resp.Provider != "openai" {
		t.Errorf("provider = %q, want openai", resp.Provider)
	}
	if resp.KeyNumber != 1 {
		t.Errorf("keyNumber = %d, want 1", resp.KeyNumber)
	}
}

func TestForwardRetryOn5xx(t *testing.T) {
	// 500 then 200: the forwarder retries and succeeds.
	doer := &fakeDoer{script: []fakeResp{{status: 500}, {status: 200, body: []byte(`{"ok":true}`)}}}
	fp, _ := newFakeProvider("openai")
	f := &Forwarder{Doer: doer, Providers: fp}
	req := &Request{Method: "POST", Path: "/x", Body: []byte(`{"model":"gpt-4o"}`), Model: "gpt-4o", Provider: "openai", Headers: http.Header{}}
	resp, err := f.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200 after retry", resp.Status)
	}
	if resp.AttemptCount != 2 {
		t.Errorf("attempts = %d, want 2", resp.AttemptCount)
	}
}

func TestForwardClientErrorNoRetry(t *testing.T) {
	// 400 is non-retryable: forwarded immediately with no retry.
	doer := &fakeDoer{script: []fakeResp{{status: 400, body: []byte(`{"error":"bad"}`)}}}
	fp, _ := newFakeProvider("openai")
	f := &Forwarder{Doer: doer, Providers: fp}
	req := &Request{Method: "POST", Path: "/x", Body: []byte(`{"model":"gpt-4o"}`), Model: "gpt-4o", Provider: "openai", Headers: http.Header{}}
	resp, err := f.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != 400 {
		t.Errorf("status = %d, want 400", resp.Status)
	}
	if resp.AttemptCount != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 400)", resp.AttemptCount)
	}
	if len(doer.calls) != 1 {
		t.Errorf("doer calls = %d, want 1", len(doer.calls))
	}
}

func TestForwardTransportErrorRetries(t *testing.T) {
	// A transport error then a 200.
	doer := &fakeDoer{script: []fakeResp{{transportErr: "connect ECONNREFUSED"}, {status: 200, body: []byte(`{"ok":true}`)}}}
	fp, _ := newFakeProvider("openai")
	f := &Forwarder{Doer: doer, Providers: fp}
	req := &Request{Method: "POST", Path: "/x", Body: []byte(`{"model":"gpt-4o"}`), Model: "gpt-4o", Provider: "openai", Headers: http.Header{}}
	resp, _ := f.Do(context.Background(), req)
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200 after transport retry", resp.Status)
	}
}

// TestForwardProxyLastErrorRecorded verifies that when a proxy is used and the
// fetch throws a transport error, RecordProxyError and SetProxyLastError are
// both called, the latter carrying the transport error message. This pins
// the wiring that populates /_status LastError.
func TestForwardProxyLastErrorRecorded(t *testing.T) {
	doer := &fakeDoer{script: []fakeResp{{transportErr: "dial tcp: connection refused"}, {status: 200, body: []byte(`{"ok":true}`)}}}
	fp, fh := newFakeProvider("openai")
	fh.ResolveProxyFn = func(slot, cycle int, sel Selection) ProxySelection {
		return ProxySelection{URL: "http://proxy:8080", Number: 1}
	}
	var lastErrURL, lastErrMsg string
	var proxyErrURL string
	fh.RecordProxyErrorFn = func(url string) { proxyErrURL = url }
	fh.SetProxyLastErrorFn = func(url, msg string) { lastErrURL = url; lastErrMsg = msg }
	f := &Forwarder{Doer: doer, Providers: fp}
	req := &Request{Method: "POST", Path: "/x", Body: []byte(`{"model":"gpt-4o"}`), Model: "gpt-4o", Provider: "openai", Headers: http.Header{}}
	resp, _ := f.Do(context.Background(), req)
	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if proxyErrURL != "http://proxy:8080" {
		t.Errorf("RecordProxyError url = %q, want http://proxy:8080", proxyErrURL)
	}
	if lastErrURL != "http://proxy:8080" {
		t.Errorf("SetProxyLastError url = %q, want http://proxy:8080", lastErrURL)
	}
	if lastErrMsg != "dial tcp: connection refused" {
		t.Errorf("SetProxyLastError msg = %q, want the transport error", lastErrMsg)
	}
}

// TestSSEBreakerOpenForwardsErrorChunk pins the behavior the refactor fixes:
// an SSE first-chunk 5xx error envelope (UpstreamOutage category) with the
// breaker OPEN must be forwarded immediately without burning all retries.
// The old sseRetry had no breaker handling and silently continued.
func TestSSEBreakerOpenForwardsErrorChunk(t *testing.T) {
	errorChunk := []byte("data: {\"error\":{\"message\":\"internal server error\"}}\n\n")
	doer := &fakeDoer{script: []fakeResp{
		{status: 200, isSSE: true, sseChunk: errorChunk},
	}}
	fp, fh := newFakeProvider("openai")
	// Breaker is already open (tripped by a prior 5xx from another request).
	fh.BreakerOpenFn = func() bool { return true }
	var breaker5xxCalls int
	fh.BreakerOn5xxFn = func() bool { breaker5xxCalls++; return true }
	f := &Forwarder{Doer: doer, Providers: fp}
	req := &Request{
		Method:   "POST",
		Path:     "/x",
		Body:     []byte(`{"model":"gpt-4o"}`),
		Model:    "gpt-4o",
		Provider: "openai",
		Headers:  http.Header{"Accept": []string{"text/event-stream"}},
	}
	resp, err := f.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	// The error chunk is forwarded as 200 (forwardSSEErrorChunk).
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200 (forwarded error chunk)", resp.Status)
	}
	if !resp.Stream {
		t.Fatal("expected Stream=true")
	}
	// The breaker was open, so no retry: exactly one attempt.
	if resp.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1 (breaker open should not retry)", resp.AttemptCount)
	}
	if len(doer.calls) != 1 {
		t.Errorf("doer calls = %d, want 1", len(doer.calls))
	}
	// BreakerOn5xx must NOT have been called when the breaker is already open.
	if breaker5xxCalls != 0 {
		t.Errorf("BreakerOn5xx called %d times, want 0 (breaker already open)", breaker5xxCalls)
	}
	// The streamed body contains the error chunk.
	out, _ := io.ReadAll(resp.StreamReader)
	if !bytes.Contains(out, []byte("internal server error")) {
		t.Errorf("streamed body does not contain error chunk: %s", out)
	}
}

// TestSSEKeyRateLimitedThenCleanChunk pins: an SSE error chunk that is
// KeyRateLimited, then a clean SSE chunk on retry, asserts AttemptCount==2
// and the response streams successfully.
func TestSSEKeyRateLimitedThenCleanChunk(t *testing.T) {
	rateLimitChunk := []byte("data: {\"error\":{\"message\":\"rate_limit_exceeded\"}}\n\n")
	cleanChunk := []byte("data: {\"choices\": [{\"delta\": {\"content\": \"hello\"}}]}\n\n")
	doer := &fakeDoer{script: []fakeResp{
		{status: 200, isSSE: true, sseChunk: rateLimitChunk},
		{status: 200, isSSE: true, sseChunk: cleanChunk},
	}}
	fp, _ := newFakeProvider("openai")
	f := &Forwarder{Doer: doer, Providers: fp}
	req := &Request{
		Method:   "POST",
		Path:     "/x",
		Body:     []byte(`{"model":"gpt-4o"}`),
		Model:    "gpt-4o",
		Provider: "openai",
		Headers:  http.Header{"Accept": []string{"text/event-stream"}},
	}
	resp, err := f.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.AttemptCount != 2 {
		t.Errorf("AttemptCount = %d, want 2", resp.AttemptCount)
	}
	if len(doer.calls) != 2 {
		t.Errorf("doer calls = %d, want 2", len(doer.calls))
	}
	if !resp.Stream {
		t.Fatal("expected Stream=true")
	}
	// The streamed body contains the clean chunk content.
	out, _ := io.ReadAll(resp.StreamReader)
	if !bytes.Contains(out, []byte("hello")) {
		t.Errorf("streamed body does not contain clean chunk: %s", out)
	}
}

func TestURLDedup(t *testing.T) {
	cases := []struct{ base, path, query, want string }{
		{"https://api.example.com/v1", "/v1/chat/completions", "", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/v1", "/v1alpha/foo", "", "https://api.example.com/v1/v1alpha/foo"},
		{"https://api.example.com/v1/", "/v1/chat", "", "https://api.example.com/v1/chat"},
		{"https://example.com/api/v1", "/v1/chat", "", "https://example.com/api/v1/v1/chat"},
		{"https://example.com", "/chat", "", "https://example.com/chat"},
		{"https://api.example.com/v1", "/chat", "stream=true", "https://api.example.com/v1/chat?stream=true"},
	}
	for i, c := range cases {
		got, err := buildUpstreamURL(c.base, c.path, c.query)
		if err != nil {
			t.Errorf("case %d: %v", i, err)
			continue
		}
		if got != c.want {
			t.Errorf("case %d: got %q, want %q", i, got, c.want)
		}
	}
}

func TestHeaderSubstitution(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-client")
	h.Set("X-Api-Key", "sk-client") // duplicate, stripped after the value no longer matches
	substituteHeaders(h, "sk-client", "sk-provider")
	if h.Get("Authorization") != "Bearer sk-provider" {
		t.Errorf("Authorization = %q, want Bearer sk-provider", h.Get("Authorization"))
	}
	// X-Api-Key value was sk-client, replaced with the provider key.
	if h.Get("X-Api-Key") != "sk-provider" {
		t.Errorf("X-Api-Key = %q, want sk-provider", h.Get("X-Api-Key"))
	}
}

func TestHeaderSubstitutionStripsNonMatching(t *testing.T) {
	// Authorization: Bearer A matches, and x-api-key: B does not match, so it is
	// stripped.
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-client")
	h.Set("X-Api-Key", "sk-other")
	substituteHeaders(h, "sk-client", "sk-provider")
	if h.Get("Authorization") != "Bearer sk-provider" {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
	if h.Get("X-Api-Key") != "" {
		t.Errorf("X-Api-Key should be stripped, got %q", h.Get("X-Api-Key"))
	}
}

func TestFallbackModelRewrite(t *testing.T) {
	body := []byte(`{"model":"gpt-4","prompt":"hello"}`)
	out, did, err := rewriteModelField(body, "gpt-4", "gpt-4o")
	if err != nil || !did {
		t.Fatalf("rewrite: did=%v err=%v", did, err)
	}
	if !strings.Contains(string(out), `"model":"gpt-4o"`) {
		t.Errorf("out = %s", out)
	}
	if !strings.Contains(string(out), `"prompt":"hello"`) {
		t.Errorf("other fields lost: %s", out)
	}
}

func TestFallbackModelNoMatch(t *testing.T) {
	body := []byte(`{"model":"claude-3"}`)
	out, did, err := rewriteModelField(body, "gpt-4", "gpt-4o")
	if err != nil || did {
		t.Errorf("expected no rewrite, did=%v", did)
	}
	if string(out) != string(body) {
		t.Errorf("body changed: %s", out)
	}
}
