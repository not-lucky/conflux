// Package stream implements SSE handling: first-chunk peek, error-envelope
// detection, keepalive, idle watchdog, and in-stream error detection.
//
// stream imports only classify and the standard library, so it is a near-leaf
// package. The forwarder calls Peek to classify the first 64 KiB of an SSE
// response, then Pipe to copy the stream with keepalive and idle-watchdog
// framing. All SSE framing rules live behind these two operations.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/not-lucky/conflux/internal/classify"
)

// FirstChunkMax is the first-chunk inspection window, 64 KiB.
const FirstChunkMax = 64 * 1024

// Keepalive is the SSE comment line emitted to keep downstream intermediaries
// from timing out.
const Keepalive = ": keepalive\n\n"

// doneSentinel is the SSE stream-terminator marker, skipped during error
// envelope detection.
const doneSentinel = "[DONE]"

// PeekResult is the outcome of inspecting the first chunk.
type PeekResult struct {
	IsError bool
	Result  classify.Result // valid when IsError
}

// IsSSE reports whether the upstream content-type indicates an SSE stream.
// The match is case-insensitive and looks for "text/event-stream".
func IsSSE(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// ReadFirstChunk reads up to FirstChunkMax bytes from r, bounded by the TTFB
// deadline. It returns (nil, io.EOF) for an empty stream: EOF without bytes
// is an error, not a success. A transport or timeout error is returned as-is.
func ReadFirstChunk(r io.Reader, ttfb time.Duration) ([]byte, error) {
	if ttfb <= 0 {
		ttfb = 60 * time.Second
	}
	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, 0, FirstChunkMax)
	tmp := make([]byte, 8192)
	ch := make(chan readResult, 1)
	go func() {
		n, err := r.Read(tmp)
		ch <- readResult{n, err}
	}()
	deadline := time.After(ttfb)
	select {
	case res := <-ch:
		if res.err != nil && res.err != io.EOF {
			return nil, res.err
		}
		if res.n == 0 && res.err == io.EOF {
			return nil, io.EOF // empty stream
		}
		buf = append(buf, tmp[:res.n]...)
		// Read up to the inspection window. Subsequent reads are synchronous;
		// the TTFB deadline already bounded the first byte.
		for len(buf) < FirstChunkMax {
			n, err := r.Read(tmp)
			if n > 0 {
				room := FirstChunkMax - len(buf)
				if n > room {
					n = room
				}
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				if err == io.EOF {
					return buf, nil
				}
				return buf, err
			}
		}
		return buf, nil
	case <-deadline:
		return nil, errors.New("ttfb timeout: stream first-chunk deadline exceeded")
	}
}

// Peek scans the buffered first chunk for an SSE error envelope. It scans
// complete `data:` lines that fit within the first 64 KiB; a partial final
// line with no trailing newline is held back and returned as trailing so the
// caller can re-inject it before piping. A
// line is an SSE error when it parses as JSON with a top-level `error` key or
// `type == "error"`.
func Peek(buf []byte) (PeekResult, []byte) {
	// Hold back bytes after the last newline as a partial line.
	lastNL := -1
	for i := len(buf) - 1; i >= 0; i-- {
		if buf[i] == '\n' {
			lastNL = i
			break
		}
	}
	var scan, trailing []byte
	if lastNL >= 0 {
		scan = buf[:lastNL+1]
		trailing = buf[lastNL+1:]
	} else {
		// The whole window is one partial line; hold it all back.
		return PeekResult{}, buf
	}
	for _, line := range splitLines(scan) {
		payload, ok := stripDataPrefix(line)
		if !ok {
			continue
		}
		if strings.TrimSpace(payload) == doneSentinel {
			continue
		}
		obj, isErr := classify.ParseSSEPayload(payload)
		if isErr {
			return PeekResult{IsError: true, Result: classify.ClassifySSE(obj).Result}, trailing
		}
	}
	return PeekResult{}, trailing
}

// PipeOptions configures the stream pump.
type PipeOptions struct {
	KeepaliveInterval time.Duration // 0 disables keepalive
	IdleTimeout       time.Duration // 0 disables the idle watchdog
}

// Pipe copies bytes from src to dst with keepalive and idle-watchdog framing.
// It is the SSE passthrough after the first chunk is approved. The reader
// delivers complete lines and the writer flushes them, so a keepalive emitted
// between line deliveries is always a clean SSE line boundary: keepalive
// fires on the writer side independently of the reader, so it is never
// starved by a long line. In-stream error envelopes are detected for tracing
// through onError but do not retry. Cancellation through ctx aborts the
// pump. trailing is the held-back partial line from Peek, re-injected before
// src.
func Pipe(ctx context.Context, src io.Reader, dst io.Writer, trailing []byte, opts PipeOptions, onError func(classify.Category)) error {
	if len(trailing) > 0 {
		src = io.MultiReader(bytes.NewReader(trailing), src)
	}

	pumpCh := make(chan []byte, 1)
	errCh := make(chan error, 2)

	// Reader goroutine: deliver complete lines (each ReadSlice result).
	go func() {
		br := bufio.NewReaderSize(src, 128*1024)
		for {
			line, err := br.ReadSlice('\n')
			if len(line) > 0 {
				cp := append([]byte(nil), line...)
				select {
				case pumpCh <- cp:
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
				// In-stream error detection: classify, fire onError, but continue the
				// stream.
				if onError != nil {
					if payload, ok := stripDataPrefix(string(line)); ok {
						if strings.TrimSpace(payload) != doneSentinel {
							if obj, isErr := classify.ParseSSEPayload(payload); isErr {
								onError(classify.ClassifySSE(obj).Category)
							}
						}
					}
				}
			}
			if err != nil {
				if err == bufio.ErrBufferFull {
					// A long line: ReadSlice returns what fit without the newline. The
					// partial was already delivered; continue to read the remainder.
					// Keepalive interleaves only between writer flushes and never
					// splits a delivered partial.
					continue
				}
				if err == io.EOF {
					errCh <- nil
					return
				}
				errCh <- err
				return
			}
		}
	}()

	var keepaliveCh <-chan time.Time
	if opts.KeepaliveInterval > 0 {
		tk := time.NewTicker(opts.KeepaliveInterval)
		defer tk.Stop()
		keepaliveCh = tk.C
	}
	idle := time.NewTimer(idleOrFallback(opts.IdleTimeout))
	defer idle.Stop()

	writeAll := func(b []byte) error {
		_, err := dst.Write(b)
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-pumpCh:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleOrFallback(opts.IdleTimeout))
			if err := writeAll(ev); err != nil {
				return err
			}
		case <-keepaliveCh:
			if err := writeAll([]byte(Keepalive)); err != nil {
				return err
			}
		case <-idle.C:
			// Idle watchdog expired: send a terminal error chunk, then close.
			_ = writeAll([]byte("data: {\"error\":{\"message\":\"TimeoutError: stream idle timeout\"}}\n\n"))
			return errors.New("stream idle timeout")
		case err := <-errCh:
			// Drain any pending line.
			select {
			case ev := <-pumpCh:
				_ = writeAll(ev)
			default:
			}
			return err
		}
	}
}

func idleOrFallback(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return 15 * time.Second
}

// splitLines splits on \n and trims a trailing \r for \r\n handling.
func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, strings.TrimRight(string(b[start:i]), "\r"))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, strings.TrimRight(string(b[start:]), "\r"))
	}
	return out
}

// stripDataPrefix extracts the payload after a `data:` prefix, optionally
// preceded by leading whitespace and a single SP after the colon. It returns
// ok=false for blank lines, comments (`:`), and other SSE fields. The JSON
// parse and error-envelope check are delegated to classify.ParseSSEPayload.
func stripDataPrefix(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	payload := strings.TrimPrefix(trimmed, "data:")
	payload = strings.TrimPrefix(payload, " ")
	return payload, true
}
