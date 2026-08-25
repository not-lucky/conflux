package dashboard

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/keypool"
	"github.com/not-lucky/conflux/internal/metrics"
	"github.com/not-lucky/conflux/internal/redact"
	"github.com/not-lucky/conflux/internal/runtime"
	"github.com/not-lucky/conflux/internal/trace"
	"github.com/not-lucky/conflux/internal/version"
)

// buildSection dispatches to the per-section view builder.
func (d *Dashboard) buildSection(section string) any {
	switch section {
	case "overview":
		return d.buildOverview()
	case "providers":
		return d.buildProviders()
	case "keys":
		return d.buildKeys()
	case "proxies":
		return d.buildProxies()
	case "breakers":
		return d.buildBreakers()
	case "models":
		return d.buildModels()
	case "traces":
		return d.buildTraces("")
	case "trace":
		return nil // trace detail is built directly in handleTraceDetail
	}
	return nil
}

// liveOrFail returns the live snapshot; the dashboard is never mounted before
// the first Store, but guard anyway.
func (d *Dashboard) liveOrFail() *runtime.Live {
	l := d.live.Load()
	if l == nil {
		return &runtime.Live{}
	}
	return l
}

// ---------------- overview ----------------

type overviewData struct {
	Version         string
	Uptime          string
	Requests        int64
	Errors          int64
	SuccessRate     string
	ErrorRate       string
	ProviderCount   int
	ProxyCount      int
	HealthyProxies  int
	ProxiesTripped  int
	KeysActive      int
	KeysTotal       int
	KeysExhausted   int
	KeysRetired     int
	Degraded        bool
	Critical        bool
	Providers       []provBar
	ErrorCategories []errCatRow
}

type provBar struct {
	Name   string
	Counts keyCounts
}

type keyCounts struct {
	Active, Standby, Exhausted, Retired, Total int
}

type errCatRow struct {
	Provider string
	Category string
	Count    int64
}

func (d *Dashboard) buildOverview() overviewData {
	l := d.liveOrFail()
	snap := d.metrics.Snapshot()

	var keysActive, keysTotal, keysExhausted, keysRetired int
	bars := make([]provBar, 0, len(l.Config.Providers))
	for _, p := range l.Config.Providers {
		pool, ok := l.Pools[p.Name]
		if !ok {
			continue
		}
		c := pool.Counts()
		total := c.Active + c.Standby + c.Exhausted + c.Retired
		keysActive += c.Active
		keysTotal += total
		keysExhausted += c.Exhausted
		keysRetired += c.Retired
		bars = append(bars, provBar{Name: p.Name, Counts: keyCounts{
			Active: c.Active, Standby: c.Standby, Exhausted: c.Exhausted, Retired: c.Retired, Total: total,
		}})
	}

	proxyEntries := l.Health.Snapshot(l.Config.ProxyURLs())
	proxyCount := len(proxyEntries)
	healthy := 0
	for _, e := range proxyEntries {
		if e.Healthy {
			healthy++
		}
	}

	// Error categories, sorted by count desc then name for a stable top list.
	cats := make([]errCatRow, 0)
	for prov, byCat := range snap.ErrorCategories {
		for cat, n := range byCat {
			cats = append(cats, errCatRow{Provider: prov, Category: cat, Count: n})
		}
	}
	sort.Slice(cats, func(i, j int) bool {
		if cats[i].Count != cats[j].Count {
			return cats[i].Count > cats[j].Count
		}
		return cats[i].Category < cats[j].Category
	})
	if len(cats) > 12 {
		cats = cats[:12]
	}

	degraded := keysExhausted > 0 || (proxyCount > 0 && healthy < proxyCount)
	critical := keysRetired > 0 && keysActive == 0

	return overviewData{
		Version:         version.Version,
		Uptime:          formatUptime(snap.UptimeSeconds),
		Requests:        snap.RequestsTotal,
		Errors:          totalErrors(snap),
		SuccessRate:     successRate(snap.RequestsTotal, totalErrors(snap)),
		ErrorRate:       errorRate(snap.RequestsTotal, totalErrors(snap)),
		ProviderCount:   len(l.Config.Providers),
		ProxyCount:      proxyCount,
		HealthyProxies:  healthy,
		ProxiesTripped:  proxyCount - healthy,
		KeysActive:      keysActive,
		KeysTotal:       keysTotal,
		KeysExhausted:   keysExhausted,
		KeysRetired:     keysRetired,
		Degraded:        degraded,
		Critical:        critical,
		Providers:       bars,
		ErrorCategories: cats,
	}
}

func totalErrors(s metrics.Snapshot) int64 {
	var n int64
	for _, byCat := range s.ErrorCategories {
		for _, c := range byCat {
			n += c
		}
	}
	return n
}

func successRate(reqs int64, errs int64) string {
	if reqs <= 0 {
		return "—"
	}
	// Success = responses that were not classified errors. Classified errors
	// count per-attempt (retries), so this is an approximation, but useful as a
	// headline gauge.
	r := float64(reqs-errs) / float64(reqs) * 100
	if r < 0 {
		r = 0
	}
	return fmt.Sprintf("%.1f", r)
}

func errorRate(reqs int64, errs int64) string {
	if reqs <= 0 {
		return "0"
	}
	return fmt.Sprintf("%.2f", float64(errs)/float64(reqs))
}

func formatUptime(s int64) string {
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	d := time.Duration(s) * time.Second
	d = d.Round(time.Second)
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// ---------------- keys ----------------

type keysData struct {
	Providers []keyProvRow
}

type keyProvRow struct {
	Name     string
	BaseURL  string
	Strategy string
	Keys     []keyRow
}

type keyRow struct {
	KeyNumber         int
	Masked            string
	State             string
	ConsecutiveErrors int
	MaxErrors         int
	Proxy             string
	Since             string
	SinceMs           int64
}

func (d *Dashboard) buildKeys() keysData {
	l := d.liveOrFail()
	out := keysData{Providers: make([]keyProvRow, 0, len(l.Config.Providers))}
	for _, p := range l.Config.Providers {
		pool, ok := l.Pools[p.Name]
		if !ok {
			continue
		}
		snap := pool.Snapshot()
		row := keyProvRow{
			Name: p.Name, BaseURL: p.BaseURL,
			Strategy: p.KeySelection.Mode,
		}
		for i, ks := range snap.Keys {
			if i >= len(p.Keys) {
				break
			}
			row.Keys = append(row.Keys, keyRow{
				KeyNumber:         i + 1,
				Masked:            redact.Key(p.Keys[i].Value),
				State:             keyState(ks, p),
				ConsecutiveErrors: ks.ConsecutiveErrors,
				MaxErrors:         p.MaxErrors,
				Proxy:             redact.URL(p.Keys[i].Proxy),
				Since:             sinceLabel(ks),
				SinceMs:           sinceMs(ks),
			})
		}
		out.Providers = append(out.Providers, row)
	}
	return out
}

// keyState derives the active/standby/exhausted/retired label for a key,
// mirroring keypool.Counts without re-implementing the cooldown math: a key
// is exhausted only when its cooldown has not yet elapsed.
func keyState(ks keypoolKeyState, p config.Provider) string {
	if ks.Retired {
		return "retired"
	}
	if !ks.ExhaustedAt.IsZero() {
		if time.Since(ks.ExhaustedAt) < p.Cooldown {
			return "exhausted"
		}
	}
	return "active"
}

// sinceLabel/sinceMs describe when a key last entered a non-healthy state, for
// the relative-time column.
func sinceLabel(ks keypoolKeyState) string {
	var t time.Time
	if ks.Retired {
		t = ks.RetiredAt
	} else if !ks.ExhaustedAt.IsZero() && time.Since(ks.ExhaustedAt) < 0 {
		t = ks.ExhaustedAt
	} else if !ks.ExhaustedAt.IsZero() {
		t = ks.ExhaustedAt
	}
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func sinceMs(ks keypoolKeyState) int64 {
	var t time.Time
	if ks.Retired {
		t = ks.RetiredAt
	} else if !ks.ExhaustedAt.IsZero() {
		t = ks.ExhaustedAt
	}
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// keypoolKeyState aliases keypool.KeyState so the view builders can reference
// it without dashboard importing keypool at the type level — kept for clarity.
type keypoolKeyState = keypool.KeyState

// ---------------- proxies ----------------

type proxiesData struct {
	Proxies []proxyRow
	Healthy int
}

type proxyRow struct {
	URL               string
	EncodedURL        string
	Healthy           bool
	ConsecutiveErrors int
	DeadUntil         string
	LastError         string
}

func (d *Dashboard) buildProxies() proxiesData {
	l := d.liveOrFail()
	entries := l.Health.Snapshot(l.Config.ProxyURLs())
	out := proxiesData{Proxies: make([]proxyRow, 0, len(entries))}
	for _, e := range entries {
		var du string
		if !e.DeadUntil.IsZero() {
			du = e.DeadUntil.UTC().Format(time.RFC3339)
		}
		out.Proxies = append(out.Proxies, proxyRow{
			URL:               redact.URL(e.URL),
			EncodedURL:        url.QueryEscape(e.URL),
			Healthy:           e.Healthy,
			ConsecutiveErrors: e.ConsecutiveErrors,
			DeadUntil:         du,
			LastError:         e.LastError,
		})
		if e.Healthy {
			out.Healthy++
		}
	}
	return out
}

// ---------------- breakers ----------------

type breakersData struct {
	Breakers  []breakerRow
	OpenCount int
}

type breakerRow struct {
	Provider       string
	Open           bool
	Consecutive5xx int
	Threshold      int
	OpenUntil      string
}

// breakerSnapshot is a tiny accessor the dashboard needs; breaker.Breaker does
// not expose its threshold or consecutive count publicly, so this reads them
// through the package's own getters added for the dashboard. (See
// breaker/breaker.go BreakerState.)
func (d *Dashboard) buildBreakers() breakersData {
	l := d.liveOrFail()
	out := breakersData{}
	for _, p := range l.Config.Providers {
		brk, ok := l.Breakers[p.Name]
		if !ok {
			continue
		}
		st := brk.State()
		row := breakerRow{
			Provider:       p.Name,
			Open:           st.Open,
			Consecutive5xx: st.Consecutive5xx,
			Threshold:      st.Threshold,
		}
		if st.Open {
			out.OpenCount++
			if !st.OpenUntil.IsZero() {
				row.OpenUntil = st.OpenUntil.UTC().Format(time.RFC3339)
			}
		}
		out.Breakers = append(out.Breakers, row)
	}
	return out
}

// ---------------- models ----------------

type modelsData struct {
	Exact    []modelRow
	Patterns []patternRow
}

type modelRow struct {
	ID       string
	Provider string
}

type patternRow struct {
	Literal  string
	Kind     string
	Provider string
}

func (d *Dashboard) buildModels() modelsData {
	l := d.liveOrFail()
	exact := make([]modelRow, 0)
	for _, mi := range l.Registry.Enumerate() {
		exact = append(exact, modelRow{ID: mi.ID, Provider: mi.OwnedBy})
	}
	patterns := make([]patternRow, 0)
	for _, p := range l.Config.Providers {
		for _, m := range p.Models {
			switch m.Kind {
			case config.ModelExact:
				continue // already in exact
			case config.ModelPrefix:
				patterns = append(patterns, patternRow{Literal: m.Literal + "*", Kind: "prefix", Provider: p.Name})
			case config.ModelCatchAll:
				patterns = append(patterns, patternRow{Literal: "*", Kind: "catch-all", Provider: p.Name})
			}
		}
	}
	return modelsData{Exact: exact, Patterns: patterns}
}

// ---------------- providers ----------------

type providersData struct {
	Providers []provDetailRow
}

type provDetailRow struct {
	Name                 string
	BaseURL              string
	KeyCount             int
	Strategy             string
	RequestsPerKey       int
	MaxErrors            int
	Cooldown             string
	RetireOnExhaustion   bool
	ActiveWindow         int
	MaxStreamRetries     int
	Upstream5xxThreshold int
	Upstream5xxCooldown  string
	RetryMaxAttempts     int
	Models               []string
	FallbackModels       []string
}

func (d *Dashboard) buildProviders() providersData {
	l := d.liveOrFail()
	out := providersData{}
	for _, p := range l.Config.Providers {
		aw := p.ActiveWindow
		if aw == 0 {
			aw = len(p.Keys)
		}
		models := make([]string, 0, len(p.Models))
		for _, m := range p.Models {
			switch m.Kind {
			case config.ModelPrefix:
				models = append(models, m.Literal+"*")
			case config.ModelCatchAll:
				models = append(models, "*")
			default:
				models = append(models, m.Literal)
			}
		}
		fb := make([]string, 0, len(p.FallbackModels))
		for from, to := range p.FallbackModels {
			fb = append(fb, from+"→"+to)
		}
		sort.Strings(fb)
		out.Providers = append(out.Providers, provDetailRow{
			Name:                 p.Name,
			BaseURL:              p.BaseURL,
			KeyCount:             len(p.Keys),
			Strategy:             p.KeySelection.Mode,
			RequestsPerKey:       p.KeySelection.RequestsPerKey,
			MaxErrors:            p.MaxErrors,
			Cooldown:             p.Cooldown.String(),
			RetireOnExhaustion:   p.RetireOnExhaustion,
			ActiveWindow:         aw,
			MaxStreamRetries:     p.MaxStreamRetries,
			Upstream5xxThreshold: p.Upstream5xxThreshold,
			Upstream5xxCooldown:  p.Upstream5xxCooldown.String(),
			RetryMaxAttempts:     p.RetryMaxAttempts,
			Models:               models,
			FallbackModels:       fb,
		})
	}
	return out
}

// ---------------- traces ----------------

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
	root := d.tracer.Root()
	level := d.tracer.Level()
	l := d.liveOrFail()
	maxDirs := 0
	if l.Config != nil {
		maxDirs = l.Config.Logging.MaxDirs
	}
	out := tracesData{
		Root:    root,
		Level:   level.String(),
		MaxDirs: maxDirs,
	}
	if level == trace.Off {
		out.Disabled = true
		return out
	}

	traceDir := filepath.Join(root, "trace")
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		// No traces yet: return empty but not disabled.
		return out
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// Newest-first: lexicographically descending.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

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

// loadTraceRow reads meta.json (or error.json) from a trace dir to populate the
// row's provider/model/duration. Failures degrade gracefully to the dir name.
func (d *Dashboard) loadTraceRow(dirName string) traceRow {
	root := d.tracer.Root()
	row := traceRow{Dir: dirName}
	// The dir name is <timestamp>_<id>; split on the last underscore.
	if i := strings.LastIndex(dirName, "_"); i >= 0 {
		row.ID = dirName[i+1:]
		row.Timestamp = humanizeTraceTS(dirName[:i])
	}
	// Prefer meta.json (success path); fall back to error.json.
	metaPath := filepath.Join(root, "trace", dirName, "meta.json")
	if m := readMetaFile(metaPath); m != nil {
		row.Outcome = "ok"
		row.Provider = m.Provider
		row.Model = m.Model
		row.DurationMs = m.DurationMs
		row.Attempt = m.Attempt
		return row
	}
	errPath := filepath.Join(root, "error", dirName, "error.json")
	if e := readErrorFile(errPath); e != nil {
		row.Outcome = "error"
		row.Provider = e.Provider
		row.Model = e.Model
		row.DurationMs = e.DurationMs
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

// readMetaFile decodes a meta.json into a small struct.
func readMetaFile(path string) *traceMeta {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m traceMeta
	if jsonDecode(b, &m) != nil {
		return nil
	}
	return &m
}

// readErrorFile decodes an error.json into a small struct.
func readErrorFile(path string) *traceError {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var e traceError
	if jsonDecode(b, &e) != nil {
		return nil
	}
	return &e
}

type traceMeta struct {
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

type traceError struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	DurationMs int64  `json:"durationMs"`
}

// ---------------- trace detail ----------------

type traceData struct {
	ID            string
	Dir           string
	DirEnc        string
	Provider      string
	Model         string
	DurationMs    int64
	Meta          *traceMeta
	Files         []string
	Active        string
	ActiveContent string
}

// buildTraceDetail loads a single trace dir's file list and the selected
// file's contents. dirName is the trace directory base name.
func (d *Dashboard) buildTraceDetail(dirName, file string) traceData {
	root := d.tracer.Root()
	out := traceData{Dir: dirName, DirEnc: url.QueryEscape(dirName)}
	if i := strings.LastIndex(dirName, "_"); i >= 0 {
		out.ID = dirName[i+1:]
	}
	dir := filepath.Join(root, "trace", dirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	out.Files = files

	// meta.json for the header.
	if m := readMetaFile(filepath.Join(dir, "meta.json")); m != nil {
		out.Meta = m
		out.Provider = m.Provider
		out.Model = m.Model
		out.DurationMs = m.DurationMs
	} else if e := readErrorFile(filepath.Join(root, "error", dirName, "error.json")); e != nil {
		out.Provider = e.Provider
		out.Model = e.Model
		out.DurationMs = e.DurationMs
	}

	// Select the file to display: the requested one, else meta.json, else the
	// first available.
	active := file
	if active == "" {
		if m := readMetaFile(filepath.Join(dir, "meta.json")); m != nil {
			active = "meta.json"
		} else if len(files) > 0 {
			active = files[0]
		}
	}
	out.Active = active
	if active != "" {
		out.ActiveContent = readTraceFile(dir, active)
	}
	return out
}

// readTraceFile reads a trace file, truncating very large stream files for the
// UI. Streaming response.stream files can be large; cap the preview.
func readTraceFile(dir, name string) string {
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		return "(could not read " + name + ")"
	}
	const cap = 256 * 1024 // 256 KiB preview
	if len(b) > cap {
		return string(b[:cap]) + "\n\n… (truncated; see " + filepath.Join(dir, name) + ")"
	}
	// Pretty-print JSON for readability.
	if strings.HasSuffix(name, ".json") {
		var buf strings.Builder
		if jsonIndent(b, &buf) == nil {
			return buf.String()
		}
	}
	return string(b)
}

// jsonDecode is a thin wrapper over json.Unmarshal for the trace readers.
func jsonDecode(b []byte, v any) error { return json.Unmarshal(b, v) }

// jsonIndent pretty-prints a JSON byte slice into buf, returning an error on
// malformed input. Used to render trace JSON files readably.
func jsonIndent(b []byte, buf *strings.Builder) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	enc, _ := json.MarshalIndent(v, "", "  ")
	buf.Write(enc)
	return nil
}
