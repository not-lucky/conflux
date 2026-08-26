// Package classify implements the upstream error classification table and
// the SSE envelope probe.
//
// classify is a leaf package of pure functions with no I/O and no internal
// imports. Its interface is Classify(resp) Result for HTTP responses and
// ClassifySSE for SSE error envelopes. The package owns the full
// classification behavior: evaluation order, 429 rate-limiting, envelope
// JSONPath probing, the transport-error string set, the 402 versus 401/403
// split, and redirect handling.
package classify

import (
	"strings"
)

// Category is the classification enum.
type Category int

const (
	Success Category = iota
	Redirect
	SharedPoolRateLimited
	KeyRateLimited
	KeyAuthFatal
	KeyBilling
	UpstreamOutage
	ClientError
	ProxyNetworkError
	UnknownError
)

func (c Category) String() string {
	switch c {
	case Success:
		return "SUCCESS"
	case Redirect:
		return "REDIRECT"
	case SharedPoolRateLimited:
		return "SHARED_POOL_RATE_LIMITED"
	case KeyRateLimited:
		return "KEY_RATE_LIMITED"
	case KeyAuthFatal:
		return "KEY_AUTH_FATAL"
	case KeyBilling:
		return "KEY_BILLING"
	case UpstreamOutage:
		return "UPSTREAM_OUTAGE"
	case ClientError:
		return "CLIENT_ERROR"
	case ProxyNetworkError:
		return "PROXY_NETWORK_ERROR"
	case UnknownError:
		return "UNKNOWN_ERROR"
	}
	return "UNKNOWN_ERROR"
}

// IsError reports whether the Category represents an error outcome.
func (c Category) IsError() bool {
	return c != Success && c != Redirect
}

// Result is the outcome of classification.
type Result struct {
	Category  Category
	Penalize  bool // whether the selected key's health is affected
	Retryable bool // whether the request may be retried
}

// Response is the input to Classify. Body is the raw upstream response body
// and may be empty; it is parsed as JSON lazily.
type Response struct {
	Status       int
	Body         []byte
	TransportErr string // non-empty when the fetch threw before headers (TTFB or dial)
}

// Classify evaluates the classification table in order. A non-empty
// TransportErr is checked first: when its message matches the canonical
// transport-error string set or a TTFB-expiry sentinel, Classify yields
// ProxyNetworkError.
func Classify(resp Response) Result {
	// A transport error is checked before the HTTP status when the fetch threw
	// before headers.
	if resp.TransportErr != "" {
		if isTransportError(resp.TransportErr) {
			return Result{Category: ProxyNetworkError, Penalize: false, Retryable: true}
		}
		// An unknown transport error is treated as a proxy or network error too:
		// retryable with no key penalty. This is safer than burning a key on an
		// unrecognized transport failure and matches the intent that transport
		// errors are egress-attributable.
		return Result{Category: ProxyNetworkError, Penalize: false, Retryable: true}
	}

	switch {
	case resp.Status >= 200 && resp.Status <= 299:
		return Result{Category: Success}
	case resp.Status == 301, resp.Status == 302, resp.Status == 303, resp.Status == 307, resp.Status == 308:
		return Result{Category: Redirect} // forwarded, not followed
	case resp.Status == 429:
		return Result{Category: KeyRateLimited, Penalize: true, Retryable: true}
	case resp.Status == 401, resp.Status == 403:
		return Result{Category: KeyAuthFatal, Penalize: true, Retryable: true}
	case resp.Status == 402:
		return Result{Category: KeyBilling, Penalize: true, Retryable: true}
	case resp.Status >= 500 && resp.Status <= 599:
		return Result{Category: UpstreamOutage, Penalize: false, Retryable: true}
	case resp.Status >= 400 && resp.Status <= 499:
		return Result{Category: ClientError} // forwarded immediately
	default:
		// A 1xx, 600+, or any other unmatched status yields UNKNOWN_ERROR and is
		// forwarded immediately.
		return Result{Category: UnknownError}
	}
}

// buildProbeString extracts the envelope-shape fields and concatenates them
// for substring probing: message, raw, limit_source, error.type, and
// error.code, plus error.message, error.msg, and error.error.
func buildProbeString(raw map[string]any) string {
	var sb strings.Builder
	add := func(v any) {
		if s, ok := v.(string); ok && s != "" {
			sb.WriteString(s)
			sb.WriteByte(' ')
		}
	}
	add(raw["message"])
	add(raw["raw"])
	add(raw["limit_source"])
	if errObj, ok := raw["error"].(map[string]any); ok {
		add(errObj["message"])
		add(errObj["msg"])
		add(errObj["error"])
		add(errObj["type"])
		add(errObj["code"])
		add(errObj["limit_source"])
	} else if errStr, ok := raw["error"].(string); ok {
		add(errStr)
	}
	if meta, ok := raw["metadata"].(map[string]any); ok {
		add(meta["limit_source"])
		add(meta["raw"])
	}
	return sb.String()
}

// transportMarkers is the canonical transport-error string set, lowercased.
// A transport error whose message contains any of these strings is a
// PROXY_NETWORK_ERROR.
var transportMarkers = []string{
	"econnrefused", "etimedout", "econnreset", "enotfound", "eai_again",
	"connection refused", "socket hung up", "connection reset",
	"timeout", "timed out", "socks", "failed to fetch", "network error",
}

// isTransportError reports whether the transport error message matches the
// canonical set. A TTFB-expiry sentinel message is also treated as a transport
// error.
func isTransportError(msg string) bool {
	l := strings.ToLower(msg)
	for _, m := range transportMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	// A TTFB expiry is reported by the forwarder as a transport error with a
	// recognizable message.
	if strings.Contains(l, "ttfb") || strings.Contains(l, "deadline exceeded") {
		return true
	}
	return false
}

// IsTransportError is the exported form for the forwarder and tests.
func IsTransportError(msg string) bool { return isTransportError(msg) }
