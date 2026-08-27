package forward

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
)

// finalizeResponse builds the downstream Response from a successful or
// non-retryable upstream attempt. It applies the x-conflux-* diagnostic
// headers to the response. The server layer decides whether to expose them
// based on server.expose_diagnostics.
func finalizeResponse(req *Request, upReq *UpstreamRequest, ph ProviderHandle, sel Selection, psel ProxySelection, effModel string, rewrote bool, upResp *UpstreamResponse, attempt int, res attemptResult) *Response {
	policy := ph.Policy()
	var upHeaders http.Header
	if upReq != nil && upReq.Headers != nil {
		upHeaders = upReq.Headers.Clone()
	}
	if upResp == nil {
		return &Response{
			Provider:        policy.Name,
			Model:           effModel,
			ModelOriginal:   req.Model,
			UpstreamHeaders: upHeaders,
			AttemptCount:    attempt,
			DownstreamError: errors.New(res.Category.String()),
			Category:        res.Category.String(),
		}
	}
	h := upResp.Headers.Clone()
	stripHopByHop(h)

	return &Response{
		Status:          upResp.Status,
		Headers:         h,
		UpstreamHeaders: upHeaders,
		Body:            upResp.BodyBuf,
		Provider:        policy.Name,
		Model:           effModel,
		ModelOriginal:   modelOriginal(rewrote, req.Model),
		KeyNumber:       sel.KeyNumber,
		ProxyURL:        psel.URL,
		ProxyNumber:     psel.Number,
		AttemptCount:    attempt,
		Category:        res.Category.String(),
	}
}

func modelOriginal(rewrote bool, original string) string {
	if rewrote {
		return original
	}
	return ""
}

// streamReadCloser wraps an io.Reader (such as an io.MultiReader) alongside
// an underlying io.Closer so that calling Close invokes the closer.
type streamReadCloser struct {
	io.Reader
	io.Closer
}

func (s *streamReadCloser) Close() error {
	if s.Closer != nil {
		return s.Closer.Close()
	}
	return nil
}

// buildStreamResponse constructs a streaming Response that replays the
// buffered first chunk followed by the remainder of the upstream stream. The
// status is caller-supplied: for SSE it is always upResp.Status (200). category
// is the canonical classification string for the Response.Category field.
func buildStreamResponse(upResp *UpstreamResponse, chunk []byte, status int, ph ProviderHandle, sel Selection, psel ProxySelection, effModel string, rewrote bool, req *Request, upReq *UpstreamRequest, attempt int, category string, rcancel context.CancelFunc) *Response {
	policy := ph.Policy()
	h := upResp.Headers.Clone()
	stripHopByHop(h)
	var closer io.Closer
	if upResp.Body != nil {
		closer = upResp.Body
	}
	combined := &streamReadCloser{
		Reader: io.MultiReader(bytes.NewReader(chunk), upResp.Body),
		Closer: closer,
	}
	var streamReader io.ReadCloser = combined
	if rcancel != nil {
		streamReader = &cancelOnClose{ReadCloser: combined, cancel: rcancel}
	}
	var upHeaders http.Header
	if upReq != nil && upReq.Headers != nil {
		upHeaders = upReq.Headers.Clone()
	}
	return &Response{
		Status:          status,
		Headers:         h,
		UpstreamHeaders: upHeaders,
		Stream:          true,
		StreamReader:    streamReader,
		Provider:        policy.Name,
		Model:           effModel,
		ModelOriginal:   modelOriginal(rewrote, req.Model),
		KeyNumber:       sel.KeyNumber,
		ProxyURL:        psel.URL,
		ProxyNumber:     psel.Number,
		AttemptCount:    attempt,
		Category:        category,
	}
}

// buildResponse is the single entry point for building a terminal Response
// inside the retry loop. For SSE it delegates to buildStreamResponse (replaying
// the buffered first chunk); for JSON it delegates to finalizeResponse.
func (f *Forwarder) buildResponse(req *Request, upReq *UpstreamRequest, ph ProviderHandle, sel Selection, psel ProxySelection, effModel string, rewrote bool, upResp *UpstreamResponse, attempt int, res attemptResult, chunk []byte, rcancel context.CancelFunc) *Response {
	if upResp != nil && upResp.IsSSE {
		return buildStreamResponse(upResp, chunk, upResp.Status, ph, sel, psel, effModel, rewrote, req, upReq, attempt, res.Category.String(), rcancel)
	}
	return finalizeResponse(req, upReq, ph, sel, psel, effModel, rewrote, upResp, attempt, res)
}
