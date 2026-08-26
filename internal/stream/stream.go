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
	"io"
	"strings"

	"github.com/not-lucky/conflux/internal/classify"
)

// FirstChunkMax is the first-chunk inspection window, 64 KiB.
const FirstChunkMax = 64 * 1024

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

// ReadFirstChunk reads the first available chunk from r (up to FirstChunkMax
// bytes). It returns (nil, io.EOF) for an empty stream: EOF without bytes is
// an error, not a success.
func ReadFirstChunk(r io.Reader) ([]byte, error) {
	tmp := make([]byte, FirstChunkMax)
	n, err := r.Read(tmp)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n == 0 && err == io.EOF {
		return nil, io.EOF // empty stream
	}
	return tmp[:n], nil
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

// Pipe copies bytes from src to dst.
// It is the SSE passthrough after the first chunk is approved. In-stream
// error envelopes are detected for tracing through onError but do not retry.
// Cancellation through ctx aborts the pump. trailing is the held-back partial
// line from Peek, re-injected before src.
func Pipe(ctx context.Context, src io.Reader, dst io.Writer, trailing []byte, onError func(classify.Category)) error {
	if len(trailing) > 0 {
		src = io.MultiReader(bytes.NewReader(trailing), src)
	}

	br := bufio.NewReaderSize(src, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			if _, werr := dst.Write(line); werr != nil {
				return werr
			}
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
				continue
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
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
