package dashboard

import (
	"net/url"
	"strings"
	"time"

	"github.com/not-lucky/conflux/internal/trace"
)

// traces.go holds the trace-browsing views (list + detail + pagination) and
// their helper functions. It is the only part of the dashboard that reaches
// into on-disk trace state; it does so exclusively through trace.Tracer
// accessors (ListTraces / ReadTraceSummary / ListTraceFiles / ReadTraceFile),
// never by composing paths against tracer.Root() itself. This keeps the
// trace/ vs error/ subdir layout and the JSON decoding inside the trace
// package, where it belongs.
//
// Live-state views (overview, keys, proxies, breakers, models, providers)
// live in views.go.

type tracesData struct {
	Root       string
	Level      string
	MaxDirs    int
	Count      int
	PrevBefore string // marker for the "newer" link (empty when on the first page)
	NextBefore string // marker for the "older" link (empty when no older page)
	Disabled   bool
	Traces     []traceRow
}

type traceRow struct {
	ID         string
	Dir        string // base name of the trace directory
	Timestamp  string
	Outcome    string // "ok" or "error"
	Provider   string
	Model      string
	DurationMs int64
	Attempt    int
}

// buildTraces lists trace directories newest-first, paginated by a `before`
// cursor (the directory name to start *before*, exclusive). The cursor is the
// lexicographically-oldest dir name on the current page; the "older" link
// passes it as `before` to fetch the next page.
//
// Because trace dir names are timestamp-prefixed (YYYYMMDDT...) and thus
// lexicographically chronological, newest-first is reverse lexicographic
// order.
func (d *Dashboard) buildTraces(before string) tracesData {
	level := d.tracer.Level()
	l := d.liveOrFail()
	maxDirs := 0
	if l.Config != nil {
		maxDirs = l.Config.Logging.MaxDirs
	}
	out := tracesData{
		Root:    d.tracer.Root(),
		Level:   level.String(),
		MaxDirs: maxDirs,
	}
	if level == trace.Off {
		out.Disabled = true
		return out
	}

	names := d.tracer.ListTraces()
	if len(names) == 0 {
		// No traces yet: return empty but not disabled.
		return out
	}

	// Apply the `before` cursor: skip until we pass the cursor, then list.
	start := 0
	if before != "" {
		for i, n := range names {
			if n == before {
				start = i + 1
				break
			}
			if n < before {
				// before is not present (e.g. pruned): start from this smaller name.
				start = i
				break
			}
		}
	}
	end := start + tracesPerPage
	if end > len(names) {
		end = len(names)
	}
	page := names[start:end]

	rows := make([]traceRow, 0, len(page))
	for _, name := range page {
		rows = append(rows, d.loadTraceRow(name))
	}
	out.Traces = rows
	out.Count = len(rows)

	// "older" link: if there are more names after this page.
	if end < len(names) {
		// Cursor = the last (oldest) name on this page.
		out.NextBefore = page[len(page)-1]
	}
	// "newer" link: if we are not on the first page. The cursor that would
	// produce the current page's start is "one past" the page immediately
	// newer. We approximate by offering to go back to the top (before="").
	// A precise "newer" cursor requires the previous page's boundary; keep it
	// simple: show "newer" only when start > 0, linking to before="" returns to
	// the top. For multi-page back-nav we encode the first name of the page
	// that precedes this one.
	if start > 0 {
		// The page immediately newer ends at index start-1 (inclusive). Its
		// oldest name is names[start-tracesPerPage], clamped to 0.
		newerStart := start - tracesPerPage
		if newerStart < 0 {
			newerStart = 0
		}
		// The "older" cursor of that newer page would be the oldest item on the
		// page before it. For the newer link we point to before=names[newerStart]
		// so that page begins right after names[newerStart-1].
		out.PrevBefore = names[newerStart]
	}
	return out
}

// loadTraceRow reads a trace's summary (meta.json or error.json) via the trace
// package and maps it to a display row. Failures degrade gracefully to the dir
// name and timestamp.
func (d *Dashboard) loadTraceRow(dirName string) traceRow {
	row := traceRow{Dir: dirName}
	// The dir name is <timestamp>_<id>; split on the last underscore.
	if i := strings.LastIndex(dirName, "_"); i >= 0 {
		row.ID = dirName[i+1:]
		row.Timestamp = humanizeTraceTS(dirName[:i])
	}
	sum := d.tracer.ReadTraceSummary(dirName)
	row.Outcome = sum.Outcome
	row.Provider = sum.Provider()
	row.Model = sum.Model()
	row.DurationMs = sum.DurationMs()
	if sum.Meta != nil {
		row.Attempt = sum.Meta.Attempt
	}
	return row
}

// humanizeTraceTS turns the dir prefix "20060102T150405_000" into a readable
// timestamp. On parse failure it returns the input unchanged.
func humanizeTraceTS(s string) string {
	// Strip the sub-second suffix "_000" if present.
	core := s
	if i := strings.LastIndex(core, "_"); i >= 0 {
		core = core[:i]
	}
	t, err := time.Parse("20060102T150405", core)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// ---------------- trace detail ----------------

type traceData struct {
	ID            string
	Dir           string
	DirEnc        string
	Provider      string
	Model         string
	DurationMs    int64
	Meta          *trace.Meta
	Files         []string
	Active        string
	ActiveContent string
}

// buildTraceDetail loads a single trace dir's file list and the selected
// file's contents. dirName is the trace directory base name.
func (d *Dashboard) buildTraceDetail(dirName, file string) traceData {
	out := traceData{Dir: dirName, DirEnc: url.QueryEscape(dirName)}
	if i := strings.LastIndex(dirName, "_"); i >= 0 {
		out.ID = dirName[i+1:]
	}
	out.Files = d.tracer.ListTraceFiles(dirName)

	sum := d.tracer.ReadTraceSummary(dirName)
	out.Provider = sum.Provider()
	out.Model = sum.Model()
	out.DurationMs = sum.DurationMs()
	out.Meta = sum.Meta

	// Select the file to display: the requested one, else meta.json, else the
	// first available.
	active := file
	if active == "" {
		if sum.Meta != nil {
			active = "meta.json"
		} else if len(out.Files) > 0 {
			active = out.Files[0]
		}
	}
	out.Active = active
	if active != "" {
		out.ActiveContent = d.tracer.ReadTraceFile(dirName, active)
	}
	return out
}
