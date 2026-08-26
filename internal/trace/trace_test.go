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

func TestSpanJSONFormatting(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, Full, 10)
	s := tr.Open("jsonreq")
	s.WriteRequest(RequestInfo{
		Method:  "POST",
		URL:     "https://api.openai.com/v1/chat/completions",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`),
		Model:   "gpt-4o",
	})
	s.WriteResponseJSON([]byte(`{"id":"chatcmpl-123","choices":[{"message":{"content":"world"}}]}`))
	s.Close()

	// Check that request.json is structured JSON rather than a single escaped string.
	entries := tr.ListTraces()
	if len(entries) != 1 {
		t.Fatalf("traces count = %d, want 1", len(entries))
	}
	content := tr.ReadTraceFile(entries[0], "request.json")
	if strings.Contains(content, `\"messages\"`) {
		t.Errorf("request body was serialized as escaped string, want structured JSON:\n%s", content)
	}
	if !strings.Contains(content, `"model": "gpt-4o"`) {
		t.Errorf("expected parsed model inside body:\n%s", content)
	}
	if !strings.Contains(content, `"content": "hello"`) {
		t.Errorf("expected parsed message content inside body:\n%s", content)
	}

	// Check response.json formatted indentation
	respContent := tr.ReadTraceFile(entries[0], "response.json")
	if !strings.Contains(respContent, `"id": "chatcmpl-123"`) {
		t.Errorf("expected formatted response JSON:\n%s", respContent)
	}
}

func TestReadTraceFileStringifiedJSON(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, Full, 10)

	// Simulate a legacy trace file where body was stored as a string literal of JSON.
	tracePath := filepath.Join(dir, "trace", "20260101T000000_000_legacy")
	if err := os.MkdirAll(tracePath, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyJSON := `{"method":"POST","body":"{\"nested\":\"value\",\"items\":[1,2,3]}"}`
	if err := os.WriteFile(filepath.Join(tracePath, "request.json"), []byte(legacyJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	formatted := tr.ReadTraceFile("20260101T000000_000_legacy", "request.json")
	if strings.Contains(formatted, `\"nested\"`) {
		t.Errorf("ReadTraceFile failed to unpack stringified JSON body:\n%s", formatted)
	}
	if !strings.Contains(formatted, `"nested": "value"`) {
		t.Errorf("expected unpacked nested key in formatted JSON:\n%s", formatted)
	}
}

func TestReadTraceFileErrorDir(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, ErrorsOnly, 10)
	s := tr.Open("err123")
	s.WriteError(ErrorInfo{
		Provider: "anthropic",
		Model:    "claude-3-opus",
		Request: RequestInfo{
			Method: "POST",
			URL:    "https://api.anthropic.com/v1/messages",
			Body:   []byte(`{"prompt":"hi"}`),
		},
		Error: "connection reset",
	})
	s.Close()

	// Verify ListTraceFiles and ReadTraceFile find the error directory
	files := tr.ListTraceFiles("20260101T000000_000_err123")
	// Using actual dir from error folder
	errEntries, _ := os.ReadDir(filepath.Join(dir, "error"))
	if len(errEntries) != 1 {
		t.Fatalf("error dirs = %d, want 1", len(errEntries))
	}
	errDirName := errEntries[0].Name()
	files = tr.ListTraceFiles(errDirName)
	if len(files) != 1 || files[0] != "error.json" {
		t.Fatalf("ListTraceFiles for error dir = %v, want [error.json]", files)
	}
	content := tr.ReadTraceFile(errDirName, "error.json")
	if !strings.Contains(content, `"connection reset"`) {
		t.Errorf("ReadTraceFile failed to read error.json:\n%s", content)
	}
	if strings.Contains(content, `\"prompt\"`) {
		t.Errorf("error request body had escaped string:\n%s", content)
	}
}

func TestReadTraceSummaryOutcome(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, Full, 10)

	// Success trace (200)
	s1 := tr.Open("req_200")
	s1.WriteMeta(Meta{Provider: "openai", Model: "gpt-4o", Category: "200"})
	s1.Close()

	// 429 SHARED_POOL_RATE_LIMITED trace
	s2 := tr.Open("req_429_shared")
	s2.WriteMeta(Meta{Provider: "openai", Model: "gpt-4o", Category: "SHARED_POOL_RATE_LIMITED"})
	s2.Close()

	// 429 status trace
	s3 := tr.Open("req_429_status")
	s3.WriteMeta(Meta{Provider: "openai", Model: "gpt-4o", Category: "429"})
	s3.Close()

	// SUCCESS category trace
	s4 := tr.Open("req_success")
	s4.WriteMeta(Meta{Provider: "openai", Model: "gpt-4o", Category: "SUCCESS"})
	s4.Close()

	traces := tr.ListTraces()
	if len(traces) != 4 {
		t.Fatalf("expected 4 traces, got %d", len(traces))
	}

	for _, traceName := range traces {
		sum := tr.ReadTraceSummary(traceName)
		switch {
		case strings.Contains(traceName, "req_200"), strings.Contains(traceName, "req_success"):
			if sum.Outcome != "ok" {
				t.Errorf("trace %s outcome = %q, want \"ok\"", traceName, sum.Outcome)
			}
		case strings.Contains(traceName, "req_429_shared"), strings.Contains(traceName, "req_429_status"):
			if sum.Outcome != "error" {
				t.Errorf("trace %s outcome = %q, want \"error\"", traceName, sum.Outcome)
			}
		}
	}
}

func TestSpanLargeJSONBody(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, Full, 10)
	s := tr.Open("large_json")

	// Create a large JSON body (> 64 KiB, e.g. 128 KiB)
	largeText := strings.Repeat("hello world large text ", 5000)
	body := `{"model":"minimax/minimax-m3:free","input":[{"role":"user","content":"` + largeText + `"}]}`
	s.WriteRequest(RequestInfo{
		Method: "POST",
		URL:    "https://openrouter.ai/api/v1/chat/completions",
		Body:   []byte(body),
		Model:  "minimax/minimax-m3:free",
	})
	s.Close()

	entries := tr.ListTraces()
	if len(entries) != 1 {
		t.Fatalf("traces count = %d, want 1", len(entries))
	}
	content := tr.ReadTraceFile(entries[0], "request.json")
	if strings.Contains(content, `"body": "{\"model"`) {
		t.Errorf("large JSON request body was stringified rather than structured JSON:\n%s", content[:500])
	}
	if !strings.Contains(content, `"model": "minimax/minimax-m3:free"`) {
		t.Errorf("expected structured model property inside request.json body")
	}
}

func TestSpanNonJSONBody(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir, Full, 10)
	s := tr.Open("non_json")

	// Create a non-JSON body > 64 KiB
	rawBody := strings.Repeat("not json data text ", 5000)
	s.WriteRequest(RequestInfo{
		Method: "POST",
		URL:    "https://api.example.com/raw",
		Body:   []byte(rawBody),
	})
	s.Close()

	entries := tr.ListTraces()
	if len(entries) != 1 {
		t.Fatalf("traces count = %d, want 1", len(entries))
	}
	content := tr.ReadTraceFile(entries[0], "request.json")
	if !strings.Contains(content, `"body": "not json data text`) {
		t.Errorf("expected truncated string body for non-JSON payload:\n%s", content[:200])
	}
}

