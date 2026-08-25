// Package forward is the deep orchestration module.
//
// Its interface is Forwarder.Do(ctx, *Request) (*Response, error). The retry
// loop, classification, key and proxy selection, header, URL, and body
// rewrite, deadline and TTFB enforcement, anti-drain guard, and SSE
// first-chunk retry all live behind that one method. Callers pass a request
// and a context; everything else is hidden.
//
// The actual HTTP transport is a port, Doer, with two adapters: httpDoer for
// production, with http and socks5 support, TTFB via a response-header
// deadline, and an idle watchdog; and a fake for tests. This is a real seam
// between production and in-memory execution.
//
// Import direction: forward imports classify, stream, and the standard
// library. It does not import keypool, proxy, breaker, config, server, auth,
// or ratelimit; the provider handle is an interface contract and those run
// before or beside the forwarder, keeping it independent of the HTTP
// envelope and the concrete collaborators.
package forward

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/not-lucky/conflux/internal/classify"
	"github.com/not-lucky/conflux/internal/stream"
)

// Request is the client-facing request the forwarder handles.
type Request struct {
	Method      string
	Path        string      // client path, after the reserved-path check
	RawQuery    string      // the search part, forwarded verbatim
	Headers     http.Header // client headers, rewritten before forwarding
	Body        []byte      // raw request body, forwarded verbatim; the model field is probed
	ContentType string      // used for multipart model extraction
	Model       string      // extracted and trimmed model id, which drives routing
	Provider    string      // matched provider name
}

// Response is the finalized downstream response.
type Response struct {
	Status       int
	Headers      http.Header // upstream headers after hop-by-hop strip
	Body         []byte      // for JSON responses
	Stream       bool        // true for SSE, in which case Body is empty and the caller handles the stream
	StreamReader io.ReadCloser
	// Diagnostics for x-conflux-* headers. The server reads these and renders
	// them when server.expose_diagnostics is true; forward does not inject
	// x-conflux-* into Headers.
	KeyNumber       int
	ProxyURL        string
	ProxyNumber     int
	Provider        string
	Model           string // effective (post-fallback) model sent upstream
	ModelOriginal   string // original model, set when a fallback rewrite occurred
	AttemptCount    int
	DownstreamError error // terminal error to translate to a status and body
	// StreamKeepalive and StreamIdleTimeout carry the provider's stream
	// settings to the server so it can pass them to stream.Pipe. Zero for
	// non-streaming responses.
	StreamKeepalive   time.Duration
	StreamIdleTimeout time.Duration
	// Category is the canonical classification string for every terminal
	// outcome. Classified outcomes use classify.Category.String() (e.g.
	// "SUCCESS", "UPSTREAM_OUTAGE", "KEY_AUTH_FATAL"); forward-level conditions
	// use "KEY_EXHAUSTION" or "EMPTY_STREAM". Never empty for a terminal
	// Response.
	Category string
}

// Doer is the transport port. The forwarder calls it per attempt.
type Doer interface {
	Do(ctx context.Context, req *UpstreamRequest) (*UpstreamResponse, error)
}

// UpstreamRequest is what the Doer sends upstream after rewriting.
type UpstreamRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
	// ViaProxy is the resolved proxy URL; an empty string means direct.
	ViaProxy string
	// TTFB is the per-attempt response-header deadline.
	TTFB time.Duration
	// SSE indicates the client expects streaming, which sets the idle watchdog
	// on the Doer.
	SSE         bool
	IdleTimeout time.Duration
}

// UpstreamResponse is what the Doer returns. For SSE, Body is the stream and
// IsSSE is true; the headers have already been received.
type UpstreamResponse struct {
	Status  int
	Headers http.Header
	Body    io.ReadCloser
	BodyBuf []byte // buffered JSON body for classification (JSON responses)
	IsSSE   bool
	// TransportErr is set when the fetch threw before headers (dial or TTFB).
	// The error message is used by classify for PROXY_NETWORK_ERROR.
	TransportErr string
}

// Forwarder orchestrates one request through selection, rewrite, the Doer,
// classification, and the retry loop.
type Forwarder struct {
	Doer Doer
	// ProviderLookup resolves a provider name to its runtime pool+policy. The
	// app wires this to the live provider set (keypool + proxy resolver + breaker).
	Providers ProviderLookup
}

// ProviderLookup returns the runtime handle for a named provider: its key
// pool, proxy resolver inputs, policy, and fallback map. The app supplies the
// concrete implementation; forward stays decoupled from keypool, proxy, and
// breaker package types by going through this interface.
type ProviderLookup interface {
	Lookup(name string) (ProviderHandle, error)
}

// ProviderPolicy bundles a provider's resolved policy fields. It is returned
// by ProviderHandle.Policy so the forwarder reads policy without 10 getters.
type ProviderPolicy struct {
	Name              string
	BaseURL           string
	MaxAttempts       int // 0 means compute the default, which forward resolves
	ActiveWindowSize  int // used to compute the default
	MaxStreamRetries  int
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
	KeepaliveInterval time.Duration
	RequestDeadline   time.Duration
	FallbackModels    map[string]string
}

// ProviderHandle is the per-provider runtime seam: it exposes the key pool,
// proxy resolver, breaker, and policy to the forwarder through an explicit
// interface contract instead of a bag of callbacks. The app builds one
// concrete handle per provider at Build time (not per request); per-request
// state such as anti-drain and triedKeys lives in the Forwarder.Do stack
// frame, not on the handle.
type ProviderHandle interface {
	// Policy returns the resolved provider policy.
	Policy() ProviderPolicy
	// Select picks a key, keyNumber, and slotIndex for the next attempt.
	Select() (Selection, error)
	RecordSuccess(keyNumber int)
	RecordError(keyNumber int) RecordResult
	MarkExhausted(keyNumber int)
	// ResolveProxy resolves the proxy for a given slot. sel is the current
	// selection so the resolver can read the inline proxy from sel.Proxy.
	ResolveProxy(slotIndex, cycleCount int, sel Selection) ProxySelection
	RecordProxyError(url string)
	SetProxyLastError(url, msg string)
	RecordProxySuccess(url string)
	// Breaker consult and update for upstream 5xx.
	BreakerOpen() bool
	BreakerOn5xx() bool
	BreakerOn2xx()
}

// Selection is a key selection result.
type Selection struct {
	Key        string
	KeyNumber  int
	SlotIndex  int
	Proxy      string // inline proxy (keys[].proxy) from the selected key, or empty
	CycleCount int    // per-provider cycle count, used by proxy rotation
}

// RecordResult mirrors keypool.RecordResult.
type RecordResult struct {
	Exhausted bool
	Retired   bool
}

// ProxySelection is a proxy resolution result.
type ProxySelection struct {
	URL    string
	Number int
	Direct bool
}

// Errors the forwarder surfaces to the server layer.
var (
	ErrNoHealthyKey = errors.New("no healthy key in active window")
	ErrEmptyStream  = errors.New("upstream returned an empty stream")
)

// Do runs the full request lifecycle. It returns a finalized *Response on
// success or a terminal error (DownstreamError set on Response) for the
// server layer to map to a status/body.
func (f *Forwarder) Do(ctx context.Context, req *Request) (*Response, error) {
	ph, err := f.Providers.Lookup(req.Provider)
	if err != nil {
		return nil, err
	}
	policy := ph.Policy()

	// Per-request anti-drain state: a closure-local seen map keyed by the
	// auth-fatal condition. This was always per-request (the old
	// ProviderHandle.AntiDrain was a per-request closure), so moving it here
	// preserves behavior exactly and removes it from the handle interface.
	seen := map[string]bool{}
	antiDrain := func(k string) bool {
		if seen[k] {
			return true
		}
		seen[k] = true
		return false
	}

	// End-to-end deadline from admission.
	deadline := policy.RequestDeadline
	if deadline <= 0 {
		deadline = 180 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	maxAttempts := policy.MaxAttempts
	if maxAttempts <= 0 {
		// Default: min(effective_active_window_size, 3), clamped to at least 1.
		aw := policy.ActiveWindowSize
		if aw <= 0 {
			aw = 1
		}
		maxAttempts = aw
		if maxAttempts > 3 {
			maxAttempts = 3
		}
		if maxAttempts < 1 {
			maxAttempts = 1
		}
	}

	triedKeys := map[int]bool{}
	streamRetries := 0
	var lastErr error
	var lastCat category = UnknownError

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Check the deadline before each attempt.
		if err := rctx.Err(); err != nil {
			break
		}

		sel, err := ph.Select()
		if err != nil {
			// No healthy key in the active window returns 503.
			return &Response{Provider: policy.Name, DownstreamError: ErrNoHealthyKey, Category: "KEY_EXHAUSTION"}, nil
		}
		// Skip keys already tried. Penalized retries use a distinct key. A
		// PROXY_NETWORK_ERROR retry reuses the same key, so it is not added to
		// triedKeys here; it is added below only for penalized categories.
		if triedKeys[sel.KeyNumber] {
			continue
		}

		// Resolve the proxy for this attempt's slot.
		psel := ph.ResolveProxy(sel.SlotIndex, sel.CycleCount, sel)

		// Rewrite the request: headers, URL, body, and fallback model.
		upReq, effModel, rewrote, rerr := f.rewrite(req, ph, sel, psel)
		if rerr != nil {
			lastErr = rerr
			continue
		}
		upReq.ViaProxy = psel.URL
		upReq.TTFB = policy.RequestTimeout
		upReq.IdleTimeout = policy.StreamIdleTimeout
		upReq.SSE = isSSERequest(req.Headers)

		// Per-attempt context bounded by the remaining deadline.
		attCtx, attCancel := context.WithCancel(rctx)
		upResp, err := f.Doer.Do(attCtx, upReq)
		attCancel()

		// Classify. For SSE, peek the first chunk FIRST so res is the real
		// (reclassified) result — the same attemptResult the JSON path uses.
		res, chunk, sseReadErr, terminal := f.classifyAndPeek(upResp, err, psel.URL != "", policy)
		if terminal != nil {
			return terminal, nil
		}

		// SSE non-EOF read error: a small inline special case that mirrors
		// today's behavior — record a proxy error, close the body, and retry
		// with the same key (no triedKeys).
		if sseReadErr != nil {
			if psel.URL != "" {
				ph.RecordProxyError(psel.URL)
				ph.SetProxyLastError(psel.URL, sseReadErr.Error())
			}
			upResp.Body.Close()
			lastErr = sseReadErr
			lastCat = ProxyNetworkError
			continue
		}

		// Count SSE error chunks for the stream-retry budget (once per chunk).
		if upResp != nil && upResp.IsSSE && res.Category != Success {
			streamRetries++
		}

		// Proxy health recording (UNIFIED — exact current semantics).
		if psel.URL != "" {
			if res.Category == ProxyNetworkError {
				ph.RecordProxyError(psel.URL)
				msg := ""
				if err != nil {
					msg = err.Error()
				} else if upResp != nil && upResp.TransportErr != "" {
					msg = upResp.TransportErr
				}
				ph.SetProxyLastError(psel.URL, msg)
			} else if err == nil {
				ph.RecordProxySuccess(psel.URL)
			}
		}

		// UpstreamOutage (UNIFIED).
		if res.Category == UpstreamOutage {
			if ph.BreakerOpen() {
				return f.buildResponse(req, ph, sel, psel, effModel, rewrote, upResp, attempt, res, chunk), nil
			}
			ph.BreakerOn5xx()
			if upResp != nil && upResp.IsSSE {
				upResp.Body.Close()
			}
			lastErr = errors.New("upstream 5xx")
			lastCat = res.Category
			if upResp != nil && upResp.IsSSE && !sseStreamBudget(maxAttempts, attempt, policy.MaxStreamRetries, streamRetries) {
				return f.buildResponse(req, ph, sel, psel, effModel, rewrote, upResp, attempt, res, chunk), nil
			}
			continue
		}

		// Success (UNIFIED).
		if res.Category == Success {
			ph.BreakerOn2xx()
			ph.RecordSuccess(sel.KeyNumber)
			return f.buildResponse(req, ph, sel, psel, effModel, rewrote, upResp, attempt, res, chunk), nil
		}

		// Penalized / non-penalized (UNIFIED — the existing fall-through,
		// now used by SSE too).
		if applyConsequences(ph, sel, res, upResp, triedKeys, antiDrain) == consequenceForward {
			return f.buildResponse(req, ph, sel, psel, effModel, rewrote, upResp, attempt, res, chunk), nil
		}
		if !res.Retryable {
			return f.buildResponse(req, ph, sel, psel, effModel, rewrote, upResp, attempt, res, chunk), nil
		}
		if upResp != nil && upResp.IsSSE {
			upResp.Body.Close()
		}
		if upResp != nil && upResp.IsSSE && !sseStreamBudget(maxAttempts, attempt, policy.MaxStreamRetries, streamRetries) {
			return f.buildResponse(req, ph, sel, psel, effModel, rewrote, upResp, attempt, res, chunk), nil
		}
		lastErr = errors.New(res.Category.String())
		lastCat = res.Category
		continue
	}

	// Retry budget exhausted.
	return &Response{Provider: policy.Name, DownstreamError: lastErr, AttemptCount: maxAttempts, Category: lastCat.String()}, nil
}

// classifyAndPeek produces, for both JSON and SSE, the effective classification
// of one attempt. For SSE it peeks the first chunk FIRST so the returned res
// is the real (reclassified) result — Success for a clean chunk, the peeked
// error category for an error envelope. This lets one unified policy section
// handle both paths.
//
// Returns:
//   - res:     the effective attemptResult.
//   - chunk:   the buffered SSE first chunk (nil for JSON).
//   - sseReadErr: non-nil only for SSE when ReadFirstChunk returns a non-EOF
//     error; the caller handles this inline (record proxy error, close, retry).
//   - terminal: a non-nil *Response to return immediately (ErrEmptyStream 502),
//     or nil.
func (f *Forwarder) classifyAndPeek(upResp *UpstreamResponse, err error, usedProxy bool, policy ProviderPolicy) (res attemptResult, chunk []byte, sseReadErr error, terminal *Response) {
	if err != nil {
		// Doer transport/TTFB error: classify as ProxyNetworkError.
		res = f.classifyAttempt(upResp, err, usedProxy)
		return res, nil, nil, nil
	}
	if upResp == nil || !upResp.IsSSE {
		// JSON path (or nil response): classify as today.
		res = f.classifyAttempt(upResp, err, usedProxy)
		return res, nil, nil, nil
	}
	// SSE: peek the first chunk so res is the real result.
	chunk, readErr := stream.ReadFirstChunk(upResp.Body, policy.RequestTimeout)
	if readErr != nil {
		if errors.Is(readErr, io.EOF) {
			// Empty stream: terminal 502 (Status set so the server treats it
			// as a “success” with 502, not a terminal 502).
			return attemptResult{}, nil, nil, &Response{
				Provider:        policy.Name,
				DownstreamError: ErrEmptyStream,
				Status:          502,
				Category:        "EMPTY_STREAM",
			}
		}
		// Other read/timeout error: return as sseReadErr for inline handling
		// in Do (record proxy error, close body, retry with same key).
		return attemptResult{Result: classify.Result{Category: ProxyNetworkError, Penalize: false, Retryable: true}}, nil, readErr, nil
	}
	pe, _ := stream.Peek(chunk)
	if !pe.IsError {
		// Clean first chunk: Success.
		return attemptResult{Result: classify.Result{Category: Success}}, chunk, nil, nil
	}
	// SSE error envelope: reclassify using the peek result.
	return attemptResult{Result: pe.Result}, chunk, nil, nil
}

// authFatalKey returns a stable key for the anti-drain guard: the same
// auth-fatal condition that already exhausted another key, such as the same
// upstream status among 401 or 403. The SSE path does not have a clean HTTP
// status (the error is in the chunk, and the upstream status is 200), so it
// falls back to "authfatal:generic". This single helper is used by both the
// JSON and SSE paths.
func authFatalKey(upResp *UpstreamResponse) string {
	if upResp != nil && (upResp.Status == 401 || upResp.Status == 403) {
		return "authfatal:" + strconv.Itoa(upResp.Status)
	}
	return "authfatal:generic"
}

// A consequence is the outcome of applyConsequences.
type consequence int

const (
	// consequenceContinue means the loop should proceed to the budget/retry
	// check and continue the retry loop.
	consequenceContinue consequence = iota
	// consequenceForward means the anti-drain guard fired (the same auth-fatal
	// condition already exhausted another key) and the caller must forward the
	// error response now instead of retrying.
	consequenceForward
)

// applyConsequences applies the shared penalize / anti-drain / triedKeys
// side effects for a classified attempt, used by BOTH the JSON path and the
// SSE error-chunk path. It mutates triedKeys and the provider handle's key
// state. It returns consequenceForward when the anti-drain guard fires on a
// KeyAuthFatal category (the caller forwards the error and stops retrying);
// otherwise consequenceContinue.
//
// UpstreamOutage is NOT handled here: its breaker-open short-circuit and
// BreakerOn5xx call are terminal/stateful and stay at the unified policy
// section's UpstreamOutage block. applyConsequences is only reached for the
// non-UpstreamOutage penalized and
// non-penalized categories (KeyAuthFatal, KeyRateLimited, KeyBilling,
// SharedPoolRateLimited, ProxyNetworkError), matching the JSON path's fall-
// through below the UpstreamOutage and Success blocks.
func applyConsequences(ph ProviderHandle, sel Selection, res attemptResult, upResp *UpstreamResponse, triedKeys map[int]bool, antiDrain func(string) bool) consequence {
	if !res.Penalize {
		// SharedPoolRateLimited and other non-penalizing categories: no
		// triedKeys, no key penalty. ProxyNetworkError is non-penalizing too.
		return consequenceContinue
	}
	triedKeys[sel.KeyNumber] = true
	if res.Category == KeyAuthFatal {
		// Anti-drain guard: the same auth-fatal condition already drained
		// another key, so stop draining and forward.
		if antiDrain(authFatalKey(upResp)) {
			return consequenceForward
		}
		ph.MarkExhausted(sel.KeyNumber)
	} else {
		ph.RecordError(sel.KeyNumber)
	}
	return consequenceContinue
}

// sseStreamBudget reports whether retry budget remains for an SSE error
// chunk retry. Both the attempt budget (maxAttempts - attempt) and the
// stream-retry budget (MaxStreamRetries) must be positive.
func sseStreamBudget(maxAttempts, attempt, maxStreamRetries, streamRetries int) bool {
	remainingRetry := maxAttempts - attempt
	remainingStream := maxStreamRetries - streamRetries + 1
	return remainingRetry > 0 && remainingStream > 0
}

func isSSERequest(h http.Header) bool {
	// A client that sends Accept: text/event-stream is asking for streaming.
	// isSSERequest uses this to set the idle watchdog on the Doer. The actual
	// stream detection comes from the upstream response content-type.
	accept := h.Get("Accept")
	return accept != "" && strings.Contains(strings.ToLower(accept), "text/event-stream")
}
