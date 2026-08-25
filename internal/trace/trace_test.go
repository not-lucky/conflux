package trace

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpanFull(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, Full, 1000)
	s := tr.Open("abc123")
	s.WriteRequest(RequestInfo{
		Method:  "POST",
		URL:     "https://api.x.com/v1/chat?key=secret&model=gpt-4o",
		Headers: http.Header{"Authorization": []string{"Bearer sk-conflux-global-001"}},
		Body:    []byte(`{"model":"gpt-4o"}`),
		Model:   "gpt-4o",
	})
	s.WriteResponseHeaders(http.Header{"Content-Type": []string{"application/json"}})
	s.WriteResponseJSON([]byte(`{"ok":true}`))
	s.WriteMeta(Meta{Provider: "openai", Model: "gpt-4o", KeyNumber: 1, DurationMs: 12, Category: "SUCCESS", Attempt: 1})
	s.Close()

	// The trace dir should contain request.json, response_headers.json,
	// response.json, and meta.json.
	traceDir := filepath.Join(dir, "trace")
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		t.Fatalf("read trace dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("trace dirs = %d, want 1", len(entries))
	}
	files, _ := os.ReadDir(filepath.Join(traceDir, entries[0].Name()))
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name()] = true
	}
	for _, want := range []string{"request.json", "response_headers.json", "response.json", "meta.json"} {
		if !names[want] {
			t.Errorf("missing %q in trace dir", want)
		}
	}

	// request.json must redact the Authorization header and the ?key= query.
	reqBytes, _ := os.ReadFile(filepath.Join(traceDir, entries[0].Name(), "request.json"))
	reqStr := string(reqBytes)
	if strings.Contains(reqStr, "sk-conflux-global-001") {
		t.Error("Authorization not redacted in request.json")
	}
	if strings.Contains(reqStr, "key=secret") {
		t.Error("?key= not redacted in request.json URL")
	}
	if !strings.Contains(reqStr, "key=****") {
		t.Error("expected key=**** in request.json URL")
	}
}

func TestSpanErrorOnly(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, ErrorsOnly, 1000)
	s := tr.Open("abc")
	// No trace dir should be created at Open for errors_only.
	s.WriteRequest(RequestInfo{Method: "POST", URL: "https://x", Body: nil})
	s.WriteError(ErrorInfo{Provider: "openai", Model: "gpt-4o", Error: "boom", DurationMs: 5})
	s.Close()

	traceDir := filepath.Join(dir, "trace")
	if _, err := os.Stat(traceDir); !os.IsNotExist(err) {
		t.Error("trace dir should not exist in errors_only")
	}
	errDir := filepath.Join(dir, "error")
	entries, err := os.ReadDir(errDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("error dir = %v %v", entries, err)
	}
}

func TestSpanOff(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, Off, 1000)
	s := tr.Open("abc")
	s.WriteRequest(RequestInfo{Method: "POST", URL: "https://x"})
	s.WriteError(ErrorInfo{Error: "x"})
	s.Close()
	// Nothing should be written.
	for _, sub := range []string{"trace", "error"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); !os.IsNotExist(err) {
			t.Errorf("%s dir should not exist in off", sub)
		}
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, Full, 2)
	for i := 0; i < 5; i++ {
		s := tr.Open("id")
		s.WriteRequest(RequestInfo{Method: "POST", URL: "https://x"})
		s.Close()
	}
	traceDir := filepath.Join(dir, "trace")
	entries, _ := os.ReadDir(traceDir)
	if len(entries) > 2 {
		t.Errorf("trace dirs = %d, want <=2 after prune", len(entries))
	}
}
