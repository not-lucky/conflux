package dashboard

import (
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/keypool"
	"github.com/not-lucky/conflux/internal/redact"
	"github.com/not-lucky/conflux/internal/runtime"
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
		Errors:          snap.ResponseErrors,
		SuccessRate:     successRate(snap.RequestsTotal, snap.ResponseErrors),
		ErrorRate:       errorRate(snap.RequestsTotal, snap.ResponseErrors),
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

// successRate is the share of downstream responses that were not errors,
// derived from requestsTotal and responseErrors (both one-per-response), so
// the ratio is always in [0,100] and needs no clamp. Returns "—" before any
// request has been served.
func successRate(reqs int64, errs int64) string {
	if reqs <= 0 {
		return "—"
	}
	r := float64(reqs-errs) / float64(reqs) * 100
	return fmt.Sprintf("%.1f", r)
}

// errorRate is the share of downstream responses that errored, derived from
// the same one-per-response population as successRate.
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
		now := time.Now()
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
				State:             keypool.Classify(ks, p.Cooldown, now),
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

// sinceLabel/sinceMs describe when a key last entered a non-healthy state, for
// the relative-time column.
func sinceLabel(ks keypoolKeyState) string {
	var t time.Time
	if ks.Retired {
		t = ks.RetiredAt
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
		aw := p.EffectiveActiveWindow
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
