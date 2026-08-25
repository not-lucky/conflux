package forward

import (
	"bytes"
	"errors"
	"io"
)

// finalizeResponse builds the downstream Response from a successful or
// non-retryable upstream attempt. It applies the x-conflux-* diagnostic
// headers to the response. The server layer decides whether to expose them
// based on server.expose_diagnostics.
func finalizeResponse(req *Request, ph ProviderHandle, sel Selection, psel ProxySelection, effModel string, rewrote bool, upResp *UpstreamResponse, attempt int, res attemptResult) *Response {
	policy := ph.Policy()
	if upResp == nil {
		return &Response{
			Provider:        policy.Name,
			Model:           effModel,
			ModelOriginal:   req.Model,
			AttemptCount:    attempt,
			DownstreamError: errors.New(res.Category.String()),
			Category:        res.Category.String(),
		}
	}
	h := upResp.Headers.Clone()
	stripHopByHop(h)

	return &Response{
		Status:        upResp.Status,
		Headers:       h,
		Body:          upResp.BodyBuf,
		Provider:      policy.Name,
		Model:         effModel,
		ModelOriginal: modelOriginal(rewrote, req.Model),
		KeyNumber:     sel.KeyNumber,
		ProxyURL:      psel.URL,
		ProxyNumber:   psel.Number,
		AttemptCount:  attempt,
		Category:      res.Category.String(),
	}
}

func modelOriginal(rewrote bool, original string) string {
	if rewrote {
		return original
	}
	return ""
}

// buildStreamResponse constructs a streaming Response that replays the
// buffered first chunk followed by the remainder of the upstream stream. The
// status is caller-supplied: for SSE it is always upResp.Status (200). category
// is the canonical classification string for the Response.Category field.
func buildStreamResponse(upResp *UpstreamResponse, chunk []byte, status int, ph ProviderHandle, sel Selection, psel ProxySelection, effModel string, rewrote bool, req *Request, attempt int, category string) *Response {
	policy := ph.Policy()
	h := upResp.Headers.Clone()
	stripHopByHop(h)
	combined := io.NopCloser(io.MultiReader(bytes.NewReader(chunk), upResp.Body))
	return &Response{
		Status:            status,
		Headers:           h,
		Stream:            true,
		StreamReader:      combined,
		Provider:          policy.Name,
		Model:             effModel,
		ModelOriginal:     modelOriginal(rewrote, req.Model),
		KeyNumber:         sel.KeyNumber,
		ProxyURL:          psel.URL,
		ProxyNumber:       psel.Number,
		AttemptCount:      attempt,
		StreamKeepalive:   policy.KeepaliveInterval,
		StreamIdleTimeout: policy.StreamIdleTimeout,
		Category:          category,
	}
}

// buildResponse is the single entry point for building a terminal Response
// inside the retry loop. For SSE it delegates to buildStreamResponse (replaying
// the buffered first chunk); for JSON it delegates to finalizeResponse.
func (f *Forwarder) buildResponse(req *Request, ph ProviderHandle, sel Selection, psel ProxySelection, effModel string, rewrote bool, upResp *UpstreamResponse, attempt int, res attemptResult, chunk []byte) *Response {
	if upResp != nil && upResp.IsSSE {
		return buildStreamResponse(upResp, chunk, upResp.Status, ph, sel, psel, effModel, rewrote, req, attempt, res.Category.String())
	}
	return finalizeResponse(req, ph, sel, psel, effModel, rewrote, upResp, attempt, res)
}
