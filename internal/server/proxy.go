package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/not-lucky/conflux/internal/auth"
	"github.com/not-lucky/conflux/internal/forward"
	"github.com/not-lucky/conflux/internal/stream"
	"github.com/not-lucky/conflux/internal/trace"
)

// handleProxy is the lifecycle for proxied requests: client-key extraction
// and auth, body read, model extraction, provider routing, per-key rate
// limiting, forwarding, and response/tracer/metrics finalization. The three
// terminal outcomes (forwarder error, downstream Status==0, success) are
// dispatched to finishError / finishSuccess so the duplicated
// WriteError+WriteMeta+RecordError / WriteMeta+Record* logic lives in one
// place.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := fmt.Sprintf("%016x", time.Now().UnixNano())
	span := s.Tracer.Open(reqID)
	defer span.Close()
	// Load the live runtime snapshot once per request so a config reload is
	// picked up atomically: a request sees a consistent config/registry/
	// forwarder/validator set even if Reload publishes a new snapshot mid-
	// request.
	l := s.liveSnapshot()
	// 1. Client key extraction and auth.
	ck := auth.ExtractKey(r.Header)
	if !l.Validator.Validate(ck) {
		s.Metrics.RecordError("", "UNAUTHORIZED")
		writeError(w, http.StatusUnauthorized, "invalid_client_key", "missing or invalid client key")
		return
	}
	// 2. Read and cap the body.
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	// 3. Extract the model.
	modelName, _, err := extractModel(r, body)
	if err != nil {
		s.Metrics.RecordError("", "CLIENT_ERROR")
		writeError(w, http.StatusBadRequest, "bad_model", err.Error())
		return
	}
	// 4. Route to the provider.
	provName, ok := l.Registry.Match(modelName)
	if !ok {
		s.Metrics.RecordError("", "CLIENT_ERROR")
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("no provider for model %q", modelName))
		return
	}
	// 5. Rate limit: provider rate_limit_rpm takes precedence over defaults
	// rate_limit_rpm.
	rpm := 0
	if prov, ok := l.Config.ProviderByName(provName); ok && prov.RateLimitRPM > 0 {
		rpm = prov.RateLimitRPM
	} else if l.Config.Defaults.RateLimitRPM > 0 {
		rpm = l.Config.Defaults.RateLimitRPM
	}
	if rpm > 0 && !s.Limiter.Allow(ck, rpm) {
		s.Metrics.RecordError(provName, "CLIENT_RATE_LIMIT")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "per-key rate limit exceeded")
		return
	}
	// 6. Build the forward request.
	freq := &forward.Request{
		Provider:    provName,
		Model:       modelName,
		Method:      r.Method,
		Path:        r.URL.Path,
		RawQuery:    r.URL.RawQuery,
		Headers:     r.Header.Clone(),
		Body:        body,
		ContentType: r.Header.Get("Content-Type"),
	}
	// Stamp the extracted client key so forward.rewrite can compare it for
	// selective header substitution. rewrite strips this header before
	// forwarding upstream.
	if ck != "" {
		freq.Headers.Set("X-Conflux-Client-Key", ck)
	}
	// The model+provider are known now; write the redacted request trace. The
	// tracer redacts headers via redact.Headers and the URL via redactQuery,
	// and truncates the body to 64 KiB. No-op when the level is not Full.
	reqInfo := trace.RequestInfo{
		Method:   r.Method,
		URL:      r.URL.String(),
		Headers:  r.Header,
		Body:     body,
		Model:    modelName,
		Provider: provName,
	}
	span.WriteRequest(reqInfo)
	// 7. Forward.
	resp, err := l.Forwarder.Do(r.Context(), freq)
	if err != nil {
		s.finishError(errorFinish{
			span:    span,
			w:       w,
			prov:    provName,
			model:   modelName,
			resp:    nil,
			reqInfo: reqInfo,
			errMsg:  err.Error(),
			start:   start,
		})
		return
	}
	// The forwarder returns a Response with Status==0 and DownstreamError set
	// when every attempt failed terminally, such as all keys exhausted or
	// transport errors with no retry budget. Translate that to a 502 here so
	// WriteHeader(0) is never called.
	if resp.Status == 0 {
		msg := "upstream unavailable"
		if resp.DownstreamError != nil {
			msg = resp.DownstreamError.Error()
		}
		s.finishError(errorFinish{
			span:    span,
			w:       w,
			prov:    provName,
			model:   modelName,
			resp:    resp,
			reqInfo: reqInfo,
			errMsg:  msg,
			start:   start,
		})
		return
	}
	// 8. Inject x-conflux-* headers before WriteHeader, then copy the response.
	s.finishSuccess(successFinish{
		span:   span,
		w:      w,
		r:      r,
		prov:   provName,
		resp:   resp,
		expose: l.Config.Server.ExposeDiagnostics,
		start:  start,
	})
}

// errorFinish carries the data for a terminal-error finish. resp is nil only
// when forwarder.Do returned a non-nil error, which today can only be an
// unknown provider from Providers.Lookup. handleProxy pre-matches the
// provider via Registry.Match before calling the forwarder, and Registry and
// the forwarder's ProviderLookup are built from the same config, so this
// branch is a defensive contract guard for the ProviderLookup/Doer seam
// rather than a live path. The canonical category on resp.Category unifies the
// metric label and the trace Category field; when resp is nil the metric
// category is "FORWARD_ERROR" and the trace category is "UNKNOWN_PROVIDER".
type errorFinish struct {
	span    *trace.Span
	w       http.ResponseWriter
	prov    string
	model   string
	resp    *forward.Response // nil only when forwarder.Do returned a non-nil error
	reqInfo trace.RequestInfo
	errMsg  string
	start   time.Time
}

// finishError unifies the two terminal-error finish paths: a forwarder error
// (resp == nil) and a downstream Status==0 terminal response (resp != nil).
// It records the per-category error metric, writes the error trace and meta
// (stamping KeyNumber/Proxy/ProxyNumber/Attempt only when resp is non-nil),
// and emits a 502 JSON error envelope. dur and the timestamp are computed
// once and shared by WriteError and WriteMeta. Both the metric category and
// the trace Category are derived from one source: resp.Category when resp is
// non-nil, or "FORWARD_ERROR"/"UNKNOWN_PROVIDER" when it is nil.
func (s *Server) finishError(ef errorFinish) {
	dur := time.Since(ef.start).Milliseconds()
	now := time.Now().UTC().Format(time.RFC3339)

	metricsCategory := "FORWARD_ERROR"
	traceCategory := "UNKNOWN_PROVIDER"
	if ef.resp != nil {
		metricsCategory = ef.resp.Category
		traceCategory = ef.resp.Category
	}

	s.Metrics.RecordError(ef.prov, metricsCategory)
	ei := trace.ErrorInfo{
		Provider:   ef.prov,
		Model:      ef.model,
		Request:    ef.reqInfo,
		Error:      ef.errMsg,
		DurationMs: dur,
		Timestamp:  now,
	}
	meta := trace.Meta{
		Provider:   ef.prov,
		Model:      ef.model,
		DurationMs: dur,
		Timestamp:  now,
		Category:   traceCategory,
	}
	if ef.resp != nil {
		ei.KeyNumber = ef.resp.KeyNumber
		ei.Proxy = ef.resp.ProxyURL
		ei.ProxyNumber = ef.resp.ProxyNumber
		meta.KeyNumber = ef.resp.KeyNumber
		meta.Proxy = ef.resp.ProxyURL
		meta.ProxyNumber = ef.resp.ProxyNumber
		meta.Attempt = ef.resp.AttemptCount
	}
	ef.span.WriteError(ei)
	ef.span.WriteMeta(meta)
	writeError(ef.w, http.StatusBadGateway, "upstream_error", ef.errMsg)
}

// successFinish carries the data for a success finish, mirroring
// errorFinish for the success path.
type successFinish struct {
	span   *trace.Span
	w      http.ResponseWriter
	r      *http.Request
	prov   string
	resp   *forward.Response
	expose bool
	start  time.Time
}

// finishSuccess is the success finish path: write the response headers/body
// to the tracer (teeing stream responses), inject x-conflux-* diagnostics,
// copy the upstream response to the client, then write meta and record the
// request count and duration. dur and the timestamp are computed once.
func (s *Server) finishSuccess(sf successFinish) {
	dur := time.Since(sf.start).Milliseconds()
	now := time.Now().UTC().Format(time.RFC3339)
	sf.span.WriteResponseHeaders(sf.resp.Headers)
	if sf.resp.Stream {
		// Tee the stream through the tracer so the streamed bytes are captured
		// into response.stream as they flow to the client. AppendStream no-ops
		// when the span has no dir (level != Full), so this is safe at
		// ErrorsOnly/Off. Wrap in a NopCloser so the pipe's Close is a no-op;
		// the underlying reader's lifetime is owned by the forwarder's
		// buildStreamResponse reader, which copyResponse does not close when
		// the stream comes from the tee wrapper.
		//
		// Body lifetime: the upstream http.Response.Body is NOT explicitly
		// Closed on the success path by design. It is released by draining to
		// EOF (normal completion — the connection is returned to the pool) or
		// by context cancellation (client disconnect — the connection is NOT
		// pooled). NopCloser hides the real Body from copyResponse's Close; the
		// transport reclaims the connection in both cases.
		sf.resp.StreamReader = io.NopCloser(io.TeeReader(sf.resp.StreamReader, streamTraceWriter{span: sf.span}))
	} else {
		sf.span.WriteResponseJSON(sf.resp.Body)
	}
	injectConfluxHeaders(sf.w, sf.prov, sf.resp, sf.expose)
	copyResponse(sf.w, sf.r, sf.resp)
	sf.span.WriteMeta(trace.Meta{
		Provider:    sf.prov,
		Model:       sf.resp.Model,
		KeyNumber:   sf.resp.KeyNumber,
		Proxy:       sf.resp.ProxyURL,
		ProxyNumber: sf.resp.ProxyNumber,
		DurationMs:  dur,
		Timestamp:   now,
		Category:    strconv.Itoa(sf.resp.Status),
		Attempt:     sf.resp.AttemptCount,
	})
	s.Metrics.RecordRequest(sf.prov, sf.resp.Model, sf.resp.Status)
	s.Metrics.RecordDuration(sf.prov, float64(dur))
}

func extractModel(r *http.Request, body []byte) (model string, isMultipart bool, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		mr := multipart.NewReader(strings.NewReader(string(body)), boundary(ct))
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				return "", true, perr
			}
			if part.FormName() == "model" {
				b, _ := io.ReadAll(io.LimitReader(part, 1024))
				return strings.TrimSpace(string(b)), true, nil
			}
		}
		return "", true, errors.New("model field not found in multipart")
	}
	var probe struct {
		Model string `json:"model"`
	}
	if len(body) > 0 {
		if jerr := json.Unmarshal(body, &probe); jerr == nil {
			return probe.Model, false, nil
		}
	}
	return "", false, nil
}

func boundary(ct string) string {
	i := strings.Index(ct, "boundary=")
	if i < 0 {
		return ""
	}
	return strings.Trim(ct[i+len("boundary="):], "\"")
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	return b, err
}

func copyResponse(w http.ResponseWriter, r *http.Request, resp *forward.Response) {
	for k, vs := range resp.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if resp.Stream {
		w.WriteHeader(resp.Status)
		fw := flushWriter{w: w}
		_ = stream.Pipe(r.Context(), resp.StreamReader, fw, nil,
			stream.PipeOptions{
				KeepaliveInterval: resp.StreamKeepalive,
				IdleTimeout:       resp.StreamIdleTimeout,
			}, nil)
		if resp.StreamReader != nil {
			resp.StreamReader.Close()
		}
		return
	}
	w.WriteHeader(resp.Status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
}

func injectConfluxHeaders(w http.ResponseWriter, prov string, resp *forward.Response, expose bool) {
	if !expose {
		return
	}
	w.Header().Set("X-Conflux-Provider", prov)
	if resp.Model != "" {
		w.Header().Set("X-Conflux-Model", resp.Model)
	}
	if resp.ModelOriginal != "" {
		w.Header().Set("X-Conflux-Model-Original", resp.ModelOriginal)
	}
	if resp.KeyNumber > 0 {
		w.Header().Set("X-Conflux-Key", strconv.Itoa(resp.KeyNumber))
	}
	if resp.ProxyURL != "" {
		w.Header().Set("X-Conflux-Proxy", strconv.Itoa(resp.ProxyNumber))
	}
	if resp.AttemptCount > 0 {
		w.Header().Set("X-Conflux-Attempt", strconv.Itoa(resp.AttemptCount))
	}
	if resp.DownstreamError != nil {
		w.Header().Set("X-Conflux-Error", resp.DownstreamError.Error())
	}
}

// flushWriter wraps an http.ResponseWriter and flushes after every Write so
// stream.Pipe's output reaches the client immediately.
type flushWriter struct {
	w http.ResponseWriter
}

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

// streamTraceWriter is the write side of an io.TeeReader wrapping a streaming
// response: every byte flowing to the client is also appended to the span's
// response.stream trace file. AppendStream is best-effort and no-ops when the
// span has no trace dir (level != Full), so this writer never errors and never
// perturbs the client stream
type streamTraceWriter struct {
	span *trace.Span
}

func (w streamTraceWriter) Write(p []byte) (int, error) {
	w.span.AppendStream(p)
	return len(p), nil
}

func writeError(w http.ResponseWriter, code int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": kind, "message": msg},
	})
}
