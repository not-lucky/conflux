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
	_, err := ReadFirstChunk(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Errorf("empty stream err = %v, want io.EOF", err)
	}
}

func TestReadFirstChunkNonBlockingInitialRead(t *testing.T) {
	// A stream that yields an initial small chunk and then delays must return
	// the first chunk immediately without waiting for a second read.
	r := &firstChunkThenSlowReader{
		first: []byte("data: {\"test\": 1}\n\n"),
	}
	chunk, err := ReadFirstChunk(r)
	if err != nil {
		t.Fatalf("ReadFirstChunk: %v", err)
	}
	if string(chunk) != "data: {\"test\": 1}\n\n" {
		t.Errorf("chunk = %q, want %q", string(chunk), "data: {\"test\": 1}\n\n")
	}
}

type firstChunkThenSlowReader struct {
	first []byte
	read  bool
}

func (r *firstChunkThenSlowReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		n := copy(p, r.first)
		return n, nil
	}
	time.Sleep(5 * time.Second)
	return 0, io.EOF
}

type slowReader struct{}

func (slowReader) Read([]byte) (int, error) {
	time.Sleep(5 * time.Second)
	return 0, io.EOF
}

func TestPipePassthrough(t *testing.T) {
	src := strings.NewReader("data: hello\n\ndata: world\n\n")
	var dst bytes.Buffer
	err := Pipe(context.Background(), src, &dst, nil, nil)
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
	err := Pipe(context.Background(), src, &dst, []byte("data: hello"), nil)
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	if dst.String() != "data: hello world\n\n" {
		t.Errorf("dst = %q", dst.String())
	}
}
