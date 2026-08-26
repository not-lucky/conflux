package forward

import (
	"net/http"

	"github.com/not-lucky/conflux/internal/classify"
)

// category wraps classify.Category so the retry loop reads cleanly.
type category = classify.Category

const (
	Success               = classify.Success
	Redirect              = classify.Redirect
	SharedPoolRateLimited = classify.SharedPoolRateLimited
	KeyRateLimited        = classify.KeyRateLimited
	KeyAuthFatal          = classify.KeyAuthFatal
	KeyBilling            = classify.KeyBilling
	UpstreamOutage        = classify.UpstreamOutage
	ClientError           = classify.ClientError
	ProxyNetworkError     = classify.ProxyNetworkError
	UnknownError          = classify.UnknownError
)

// attemptResult is the classification of one upstream attempt.
type attemptResult struct {
	classify.Result
}

// classifyAttempt maps a Doer outcome into a classify.Result. err is the
// Doer error, transport or TTFB; upResp is the response, or nil when err is
// set. usedProxy reports whether a proxy was used, for proxy health decisions
// handled by the caller.
func (f *Forwarder) classifyAttempt(upResp *UpstreamResponse, err error, usedProxy bool) attemptResult {
	if err != nil {
		// A transport error from dial or TTFB. classify treats transport
		// messages as PROXY_NETWORK_ERROR.
		return attemptResult{Result: classify.Classify(classify.Response{TransportErr: err.Error()})}
	}
	if upResp == nil {
		return attemptResult{Result: classify.Result{Category: UnknownError}}
	}
	return attemptResult{Result: classify.Classify(classify.Response{
		Status: upResp.Status,
		Body:   bodyForClassify(upResp),
	})}
}

// bodyForClassify returns the buffered body for classification.
// For SSE responses, where IsSSE is true, the body is the stream and is not
// read for status-based classification: the SSE path handles its own
// classification.
func bodyForClassify(upResp *UpstreamResponse) []byte {
	if upResp.IsSSE {
		return nil
	}
	// The Doer for JSON responses should stash a peeked body; up to 16 KiB is
	// read for classification. To keep the Doer simple, JSON responses read the
	// whole body into a buffer accessible here through upResp.BodyBuf.
	if upResp.BodyBuf != nil {
		return upResp.BodyBuf
	}
	return nil
}

// hopByHop lists the hop-by-hop headers stripped before forwarding.
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding",
	"Proxy-Authorization", "Te", "Trailer", "Upgrade",
}

// stripHopByHop removes hop-by-hop headers in place.
func stripHopByHop(h http.Header) {
	for _, k := range hopByHop {
		h.Del(k)
	}
	// Host is replaced by the Doer from the upstream URL and is never
	// forwarded.
	h.Del("Host")
}
