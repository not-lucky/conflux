package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestIsSSE(t *testing.T) {
	cases := map[string]bool{
		"text/event-stream":                true,
		"Text/Event-Stream":                true,
		"text/event-stream; charset=utf-8": true,
		"application/json":                 false,
		"":                                 false,
	}
	for in, want := range cases {
		if got := IsSSE(in); got != want {
			t.Errorf("IsSSE(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPeekDetectsError(t *testing.T) {
	chunk := []byte("data: {\"error\":{\"message\":\"rate_limit_exceeded\"}}\n\n")
	res, trailing := Peek(chunk)
	if !res.IsError {
		t.Fatal("expected error envelope detected")
	}
	if res.Result.Category.String() != "KEY_RATE_LIMITED" {
		t.Errorf("category = %s, want KEY_RATE_LIMITED", res.Result.Category)
	}
	if len(trailing) != 0 {
		t.Errorf("trailing = %q, want empty", trailing)
	}
}

func TestPeekIgnoresDone(t *testing.T) {
	chunk := []byte("data: [DONE]\n\n")
	res, _ := Peek(chunk)
	if res.IsError {
		t.Fatal("[DONE] must not be detected as error")
	}
}

func TestPeekIgnoresCompletionText(t *testing.T) {
	// {"content":"Error 401 in story"} has no top-level error, so it is not an
	// error.
	chunk := []byte("data: {\"content\":\"Error 401 in story\"}\n\n")
	res, _ := Peek(chunk)
	if res.IsError {
		t.Fatal("completion text without error envelope must not be detected")
	}
}

func TestPeekPartialLineHeldBack(t *testing.T) {
	// With no trailing newline, the whole buffer is a partial line and is held
	// back.
	chunk := []byte("data: {\"error\":{\"message\":\"x\"}}") // no \n
	res, trailing := Peek(chunk)
	if res.IsError {
		t.Error("partial line must not be classified")
	}
	if string(trailing) != string(chunk) {
		t.Errorf("trailing = %q, want the whole chunk held back", trailing)
	}
}

func TestPeekClassifiesErrorAtBoundary(t *testing.T) {
	// A complete data: line within the first 64 KiB is detected.
	chunk := []byte("data: {\"error\":{\"type\":\"authentication_error\"}}\n\n")
	res, _ := Peek(chunk)
	if !res.IsError {
		t.Fatal("expected auth-fatal detected")
	}
	if res.Result.Penalize != true {
		t.Error("auth-fatal should penalize")
	}
}

func TestReadFirstChunkEmptyStream(t *testing.T) {
	_, err := ReadFirstChunk(bytes.NewReader(nil), time.Second)
	if !errors.Is(err, io.EOF) {
		t.Errorf("empty stream err = %v, want io.EOF", err)
	}
}

func TestReadFirstChunkTimeout(t *testing.T) {
	// A reader that never delivers hits the TTFB deadline.
	r := &slowReader{}
	_, err := ReadFirstChunk(r, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected ttfb timeout")
	}
	if !strings.Contains(err.Error(), "ttfb") {
		t.Errorf("err = %v, want ttfb timeout", err)
	}
}

type slowReader struct{}

func (slowReader) Read([]byte) (int, error) {
	time.Sleep(5 * time.Second)
	return 0, io.EOF
}

func TestPipePassthrough(t *testing.T) {
	src := strings.NewReader("data: hello\n\ndata: world\n\n")
	var dst bytes.Buffer
	err := Pipe(context.Background(), src, &dst, nil, PipeOptions{}, nil)
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	if dst.String() != "data: hello\n\ndata: world\n\n" {
		t.Errorf("dst = %q", dst.String())
	}
}

func TestPipeTrailingReinjected(t *testing.T) {
	// The trailing partial line is re-injected before src.
	src := strings.NewReader(" world\n\n")
	var dst bytes.Buffer
	err := Pipe(context.Background(), src, &dst, []byte("data: hello"), PipeOptions{}, nil)
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	if dst.String() != "data: hello world\n\n" {
		t.Errorf("dst = %q", dst.String())
	}
}

func TestPipeKeepalive(t *testing.T) {
	// A slow source with a short keepalive interval emits keepalives.
	src := &intervalReader{lines: [][]byte{[]byte("data: a\n\n")}, delay: 100 * time.Millisecond}
	var dst bytes.Buffer
	err := Pipe(context.Background(), src, &dst, nil, PipeOptions{
		KeepaliveInterval: 30 * time.Millisecond,
		IdleTimeout:       5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	if !strings.Contains(dst.String(), Keepalive) {
		t.Errorf("expected keepalive in output, got %q", dst.String())
	}
	if !strings.Contains(dst.String(), "data: a\n\n") {
		t.Errorf("expected data line, got %q", dst.String())
	}
}

func TestPipeIdleTimeout(t *testing.T) {
	// A source that stalls after one line triggers the idle timeout.
	src := &intervalReader{lines: [][]byte{[]byte("data: a\n\n")}, delay: 0, stallAfter: true}
	var dst bytes.Buffer
	err := Pipe(context.Background(), src, &dst, nil, PipeOptions{
		IdleTimeout: 40 * time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("expected idle timeout error")
	}
	if !strings.Contains(dst.String(), "TimeoutError") {
		t.Errorf("expected terminal error chunk, got %q", dst.String())
	}
}

// intervalReader emits lines with optional delays and an optional final stall.
type intervalReader struct {
	lines      [][]byte
	delay      time.Duration
	stallAfter bool
	off        int
}

func (r *intervalReader) Read(p []byte) (int, error) {
	if r.off >= len(r.lines) {
		if r.stallAfter {
			time.Sleep(5 * time.Second)
			return 0, io.EOF
		}
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	line := r.lines[r.off]
	n := copy(p, line)
	r.off++
	if n < len(line) {
		// The line was not fully copied. Rewinding by pushing the remainder
		// back is not supported here, so tests keep lines small enough to
		// fit.
	}
	return n, nil
}
