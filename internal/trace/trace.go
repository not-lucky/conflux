// Package trace implements on-disk request tracing: per-request trace
// directories, error traces, header and body redaction, and retention
// pruning.
//
// trace is an observer package: it records what other packages hand it; it
// never drives domain logic. It imports only the standard library and redact;
// the Level enum is defined locally.
package trace

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/not-lucky/conflux/internal/redact"
)

// Level is the logging level.
type Level int

const (
	Off Level = iota
	ErrorsOnly
	Full
)

// ParseLevel maps the config string to a Level.
func ParseLevel(s string) Level {
	switch s {
	case "full":
		return Full
	case "errors_only":
		return ErrorsOnly
	case "off":
		return Off
	default:
		return Full
	}
}

// String renders the level for display (the dashboard trace viewer).
func (l Level) String() string {
	switch l {
	case Full:
		return "full"
	case ErrorsOnly:
		return "errors_only"
	case Off:
		return "off"
	default:
		return "unknown"
	}
}

// Tracer writes per-request traces and prunes retention. Tracer is safe for
// concurrent use; each Span is single-goroutine, the request handler.
type Tracer struct {
	root    string // base logs dir, default "./logs"
	level   Level
	maxDirs int

	mu      sync.Mutex
	pruning bool
}

// New builds a Tracer. root is the logs base, such as "./logs"; trace and
// error subdirectories are created under it.
func New(root string, level Level, maxDirs int) *Tracer {
	if root == "" {
		root = "./logs"
	}
	return &Tracer{root: root, level: level, maxDirs: maxDirs}
}

// Level returns the configured level.
func (t *Tracer) Level() Level { return t.level }

// Root returns the base logs directory ("./logs" by default). The dashboard
// uses it to browse on-disk trace and error directories.
func (t *Tracer) Root() string { return t.root }

// TraceSummary is the merged metadata for one trace directory, read from
// meta.json (success) or error.json (failure). Exactly one of Meta/Error is
// non-nil. Outcome is "ok" or "error". Dir is the directory base name.
//
// This is the dashboard-facing accessor: it hides the trace/ vs error/
// subdir split and the file-decoding details behind the trace package.
type TraceSummary struct {
	Dir     string
	Outcome string // "ok" when Meta != nil, "error" when Error != nil
	Meta    *Meta
	Error   *ErrorInfo
}

// Provider returns the provider of the summary, whichever record is present.
func (s TraceSummary) Provider() string {
	if s.Meta != nil {
		return s.Meta.Provider
	}
	if s.Error != nil {
		return s.Error.Provider
	}
	return ""
}

// Model returns the model of the summary, whichever record is present.
func (s TraceSummary) Model() string {
	if s.Meta != nil {
		return s.Meta.Model
	}
	if s.Error != nil {
		return s.Error.Model
	}
	return ""
}

// DurationMs returns the request duration of the summary.
func (s TraceSummary) DurationMs() int64 {
	if s.Meta != nil {
		return s.Meta.DurationMs
	}
	if s.Error != nil {
		return s.Error.DurationMs
	}
	return 0
}

// ListTraces returns the names of all trace directories under root/trace,
// sorted newest-first (reverse lexicographic — dir names are timestamp-
// prefixed, so this is chronological newest-first). It does not read the
// directory contents; pair each name with ReadTraceSummary for metadata.
// Returns nil when the trace directory does not exist yet.
func (t *Tracer) ListTraces() []string {
	entries, err := os.ReadDir(filepath.Join(t.root, "trace"))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

// ReadTraceSummary loads meta.json (success path) or, failing that,
// error.json (failure path) for one trace directory. Returns a TraceSummary
// with the appropriate field set, or a TraceSummary with only Dir populated
// when neither file exists or can be decoded (the caller degrades gracefully).
func (t *Tracer) ReadTraceSummary(dir string) TraceSummary {
	sum := TraceSummary{Dir: dir}
	if m := readJSONFile[Meta](filepath.Join(t.root, "trace", dir, "meta.json")); m != nil {
		sum.Outcome = "ok"
		sum.Meta = m
		return sum
	}
	if e := readJSONFile[ErrorInfo](filepath.Join(t.root, "error", dir, "error.json")); e != nil {
		sum.Outcome = "error"
		sum.Error = e
	}
	return sum
}

// ListTraceFiles returns the file names inside one trace directory, sorted
// lexicographically. Returns nil when the directory does not exist.
func (t *Tracer) ListTraceFiles(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(t.root, "trace", dir))
	if err != nil {
		return nil
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files
}

// ReadTraceFile reads one file from a trace directory for the dashboard viewer.
// Large files (notably response.stream) are truncated to a 256 KiB preview
// with a notice. JSON files are pretty-printed. Read errors return a
// human-readable placeholder instead of propagating, since the viewer degrades
// gracefully.
func (t *Tracer) ReadTraceFile(dir, name string) string {
	path := filepath.Join(t.root, "trace", dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		return "(could not read " + name + ")"
	}
	const preview = 256 * 1024 // 256 KiB preview
	if len(b) > preview {
		return string(b[:preview]) + "\n\n… (truncated; see " + path + ")"
	}
	if strings.HasSuffix(name, ".json") {
		var v any
		if err := json.Unmarshal(b, &v); err == nil {
			enc, _ := json.MarshalIndent(v, "", "  ")
			return string(enc)
		}
	}
	return string(b)
}

// readJSONFile decodes a JSON file into T, returning nil on any read/decode
// error. Used by the trace-summary readers.
func readJSONFile[T any](path string) *T {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v T
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return &v
}

// Span is one request trace. Open it, write fields, and Close to finalize.
type Span struct {
	tracer *Tracer
	dir    string // trace dir, may be empty when level is errors_only
	errDir string // error dir, set for failures
	opened time.Time
	id     string
}

// RequestInfo describes the request for the trace.
type RequestInfo struct {
	Method   string
	URL      string // will be redacted (?key= becomes ****)
	Headers  http.Header
	Body     []byte // truncated to 64 KiB
	Model    string
	Provider string
}

// Open starts a trace span. When the level is Off, Open returns a no-op span
// whose writes are all dropped. When the level is Full, a trace dir is created;
// when the level is ErrorsOnly, only an error dir is created on WriteError.
func (t *Tracer) Open(id string) *Span {
	now := time.Now().UTC()
	s := &Span{tracer: t, opened: now, id: id}
	if t.level >= Full {
		ts := now.Format("20060102T150405_000") + "_" + id
		s.dir = filepath.Join(t.root, "trace", ts)
		if err := os.MkdirAll(s.dir, 0o755); err == nil {
			// best-effort: writes that fail are skipped
		}
	}
	return s
}

// WriteRequest writes request.json (redacted) to the trace dir.
func (s *Span) WriteRequest(info RequestInfo) {
	if s.dir == "" {
		return
	}
	body := info.Body
	if len(body) > 64*1024 {
		body = body[:64*1024]
	}
	rec := map[string]any{
		"method":   info.Method,
		"url":      redact.QueryValues(info.URL),
		"headers":  redact.Headers(info.Headers),
		"body":     string(body),
		"model":    info.Model,
		"provider": info.Provider,
	}
	s.writeJSON(filepath.Join(s.dir, "request.json"), rec)
}

// WriteResponseHeaders writes response_headers.json.
func (s *Span) WriteResponseHeaders(h http.Header) {
	if s.dir == "" {
		return
	}
	s.writeJSON(filepath.Join(s.dir, "response_headers.json"), h)
}

// WriteResponseJSON writes response.json for a JSON response.
func (s *Span) WriteResponseJSON(body []byte) {
	if s.dir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(s.dir, "response.json"), body, 0o644)
}

// AppendStream appends SSE bytes to response.stream.
func (s *Span) AppendStream(b []byte) {
	if s.dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "response.stream"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(b)
}

// WriteMeta writes meta.json.
func (s *Span) WriteMeta(m Meta) {
	if s.dir == "" {
		return
	}
	s.writeJSON(filepath.Join(s.dir, "meta.json"), m)
}

// Meta is the meta.json content.
type Meta struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	KeyNumber   int    `json:"keyNumber"`
	Proxy       string `json:"proxy"`
	ProxyNumber int    `json:"proxyNumber"`
	DurationMs  int64  `json:"durationMs"`
	Timestamp   string `json:"timestamp"`
	Category    string `json:"category"`
	Attempt     int    `json:"attempt"`
}

// WriteError writes an error trace to
// logs/error/<ts>_<id>/error.json. It is written for failures at level Full,
// also in the trace, and at level ErrorsOnly.
func (s *Span) WriteError(e ErrorInfo) {
	if s.tracer.level < ErrorsOnly {
		return
	}
	ts := s.opened.Format("20060102T150405_000") + "_" + s.id
	dir := filepath.Join(s.tracer.root, "error", ts)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	s.writeJSON(filepath.Join(dir, "error.json"), e)
}

// ErrorInfo is the error.json content.
type ErrorInfo struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	KeyNumber   int    `json:"keyNumber"`
	Proxy       string `json:"proxy"`
	ProxyNumber int    `json:"proxyNumber"`
	Request     any    `json:"request"`
	Response    any    `json:"response"`
	Error       string `json:"error"`
	DurationMs  int64  `json:"durationMs"`
	Timestamp   string `json:"timestamp"`
}

// Close finalizes the span and triggers pruning, best-effort.
func (s *Span) Close() {
	s.tracer.prune()
}

func (s *Span) writeJSON(path string, v any) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// prune deletes the oldest subdirs beyond maxDirs in both the trace and
// error dirs. prune is best-effort: failures are logged to stderr and never
// panic.
func (t *Tracer) prune() {
	t.mu.Lock()
	if t.pruning {
		t.mu.Unlock()
		return
	}
	t.pruning = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.pruning = false
		t.mu.Unlock()
	}()

	for _, sub := range []string{"trace", "error"} {
		dir := filepath.Join(t.root, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var dirs []os.DirEntry
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, e)
			}
		}
		// Sort by name: the timestamp prefix means lexicographic order is
		// chronological.
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name() < dirs[j].Name() })
		limit := t.maxDirs
		if len(dirs) > limit {
			for _, d := range dirs[:len(dirs)-limit] {
				_ = os.RemoveAll(filepath.Join(dir, d.Name()))
			}
		}
	}
}
