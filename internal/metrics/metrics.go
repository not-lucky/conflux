// Package metrics implements the Prometheus exposition and the /_status JSON
// snapshot.
//
// metrics is an observer package: it records counters, gauges, and
// histograms that other packages report to it, and it renders them. It never
// drives domain logic. It imports only the standard library: fmt, io, sync,
// sync/atomic, and time.
package metrics

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Registry is the central metrics store. Safe for concurrent use.
type Registry struct {
	startTime time.Time

	mu sync.Mutex

	// counters
	requestsTotal      atomic.Int64                        // one per downstream response
	requestsByProvider map[string]map[int]int64            // provider -> status -> count
	requestsByModel    map[string]map[string]map[int]int64 // model -> provider -> status -> count
	errorCategories    map[string]map[string]int64         // provider -> category -> count

	// gauges
	keysGauge    map[string]map[string]int64 // provider -> state -> count (active/standby/exhausted/retired)
	proxyHealthy map[string]int64            // proxy url -> 1/0

	// histogram: request_duration_ms
	histMu sync.Mutex
	hist   map[string]*histogram // provider -> histogram

	// uptime
	uptimeStart time.Time
}

// histogram buckets, in ms: 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500,
// 5000, 10000, 30000, 60000, 300000, and +Inf.
var histBuckets = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 300000}

type histogram struct {
	counts []int64 // per bucket; len is len(histBuckets)
	sum    float64
	count  int64
}

func newHistogram() *histogram {
	return &histogram{counts: make([]int64, len(histBuckets))}
}

func (h *histogram) observe(ms float64) {
	h.sum += ms
	h.count++
	// Cumulative: counts[i] is the number of observations <= histBuckets[i].
	for i, b := range histBuckets {
		if ms <= b {
			h.counts[i]++
		}
	}
}

// New builds a Registry. startTime is used for the uptime gauge.
func New(startTime time.Time) *Registry {
	return &Registry{
		startTime:          startTime,
		uptimeStart:        startTime,
		requestsByProvider: map[string]map[int]int64{},
		requestsByModel:    map[string]map[string]map[int]int64{},
		errorCategories:    map[string]map[string]int64{},
		keysGauge:          map[string]map[string]int64{},
		proxyHealthy:       map[string]int64{},
		hist:               map[string]*histogram{},
	}
}

// RecordRequest increments the per-downstream-response counters for the final
// status, by provider and optionally by model.
func (r *Registry) RecordRequest(provider, model string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requestsTotal.Add(1)
	if r.requestsByProvider[provider] == nil {
		r.requestsByProvider[provider] = map[int]int64{}
	}
	r.requestsByProvider[provider][status]++
	if model != "" {
		if r.requestsByModel[model] == nil {
			r.requestsByModel[model] = map[string]map[int]int64{}
		}
		if r.requestsByModel[model][provider] == nil {
			r.requestsByModel[model][provider] = map[int]int64{}
		}
		r.requestsByModel[model][provider][status]++
	}
}

// RecordError increments the per-classified-error-attempt counter, for
// intermediate retries and SSE in-stream errors.
func (r *Registry) RecordError(provider, category string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.errorCategories[provider] == nil {
		r.errorCategories[provider] = map[string]int64{}
	}
	r.errorCategories[provider][category]++
}

// RecordDuration observes the request duration histogram, from admission to
// the downstream response end in ms, for a provider.
func (r *Registry) RecordDuration(provider string, ms float64) {
	r.histMu.Lock()
	defer r.histMu.Unlock()
	h, ok := r.hist[provider]
	if !ok {
		h = newHistogram()
		r.hist[provider] = h
	}
	h.observe(ms)
}

// SetKeysGauge sets the per-provider key-state counts: active, standby,
// exhausted, and retired.
func (r *Registry) SetKeysGauge(provider string, active, standby, exhausted, retired int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keysGauge[provider] == nil {
		r.keysGauge[provider] = map[string]int64{}
	}
	r.keysGauge[provider]["active"] = active
	r.keysGauge[provider]["standby"] = standby
	r.keysGauge[provider]["exhausted"] = exhausted
	r.keysGauge[provider]["retired"] = retired
}

// SetProxyHealthy sets the per-URL proxy health gauge: 1 for healthy, 0 for
// tripped. The url must be credential-stripped by the caller.
func (r *Registry) SetProxyHealthy(url string, healthy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if healthy {
		r.proxyHealthy[url] = 1
	} else {
		r.proxyHealthy[url] = 0
	}
}

// WritePrometheus renders the exposition to w.
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.histMu.Lock()
	defer r.histMu.Unlock()

	fmt.Fprintf(w, "# HELP conflux_uptime_seconds gauge\nconflux_uptime_seconds %d\n",
		int64(time.Since(r.uptimeStart).Seconds()))

	fmt.Fprintln(w, "# HELP conflux_requests_total counter, incremented once per downstream response")
	fmt.Fprintf(w, "conflux_requests_total %d\n", r.requestsTotal.Load())

	// requests_by_provider
	fmt.Fprintln(w, "# HELP conflux_requests_by_provider counter, the final downstream HTTP status per provider")
	for prov, statuses := range r.requestsByProvider {
		for status, n := range statuses {
			fmt.Fprintf(w, "conflux_requests_by_provider{provider=%q,status=%q} %d\n", prov, fmt.Sprintf("%d", status), n)
		}
	}

	// requests_by_model
	fmt.Fprintln(w, "# HELP conflux_requests_by_model counter, the final downstream HTTP status per model and provider")
	for model, provs := range r.requestsByModel {
		for prov, statuses := range provs {
			for status, n := range statuses {
				fmt.Fprintf(w, "conflux_requests_by_model{model=%q,provider=%q,status=%q} %d\n",
					model, prov, fmt.Sprintf("%d", status), n)
			}
		}
	}

	// error_categories_total
	fmt.Fprintln(w, "# HELP conflux_error_categories_total counter, one per classified error attempt")
	for prov, cats := range r.errorCategories {
		for cat, n := range cats {
			fmt.Fprintf(w, "conflux_error_categories_total{provider=%q,category=%q} %d\n", prov, cat, n)
		}
	}

	// request_duration_ms histogram
	fmt.Fprintln(w, "# HELP conflux_request_duration_ms histogram, from admission to downstream response end")
	fmt.Fprintln(w, "# TYPE conflux_request_duration_ms histogram")
	for prov, h := range r.hist {
		for i, b := range histBuckets {
			fmt.Fprintf(w, "conflux_request_duration_ms_bucket{provider=%q,le=%q} %d\n", prov, fmt.Sprintf("%g", b), h.counts[i])
		}
		fmt.Fprintf(w, "conflux_request_duration_ms_bucket{provider=%q,le=\"+Inf\"} %d\n", prov, h.count)
		fmt.Fprintf(w, "conflux_request_duration_ms_sum{provider=%q} %g\n", prov, h.sum)
		fmt.Fprintf(w, "conflux_request_duration_ms_count{provider=%q} %d\n", prov, h.count)
	}

	// keys gauge
	fmt.Fprintln(w, "# HELP conflux_keys gauge")
	for prov, states := range r.keysGauge {
		for state, n := range states {
			fmt.Fprintf(w, "conflux_keys{provider=%q,state=%q} %d\n", prov, state, n)
		}
	}

	// proxy_healthy
	fmt.Fprintln(w, "# HELP conflux_proxy_healthy gauge, 1 for healthy and 0 for tripped")
	for url, v := range r.proxyHealthy {
		fmt.Fprintf(w, "conflux_proxy_healthy{proxy=%q} %d\n", url, v)
	}
}

// StatusJSON is the /_status contract. The server layer populates the
// provider detail from the live pool snapshots; the raw counters are exposed
// here through the Recorder interface.
type StatusJSON struct {
	Version       string                 `json:"version"`
	OK            bool                   `json:"ok"`
	UptimeSeconds int64                  `json:"uptimeSeconds"`
	Proxies       map[string]ProxyHealth `json:"proxies"`
	Metrics       StatusMetrics          `json:"metrics"`
	Status        StatusDetail           `json:"status"`
}

type ProxyHealth struct {
	Healthy           bool   `json:"healthy"`
	ConsecutiveErrors int    `json:"consecutiveErrors"`
	DeadUntil         *int64 `json:"deadUntil"` // unix ms, nil when not tripped
	LastError         string `json:"lastError"`
}

type StatusMetrics struct {
	TotalRequests int64 `json:"totalRequests"`
	TotalErrors   int64 `json:"totalErrors"`
}

type StatusDetail struct {
	GlobalProxies []string       `json:"globalProxies"`
	ClientKeys    []string       `json:"clientKeys"` // masked
	Providers     map[string]any `json:"providers"`
}

// Status returns the /_status JSON, populated from the registry plus the
// caller-supplied provider and proxy detail. The caller is responsible for
// masking client keys and stripping proxy credentials.
func (r *Registry) Status(version string, detail StatusDetail, proxyHealth map[string]ProxyHealth) StatusJSON {
	r.mu.Lock()
	defer r.mu.Unlock()
	var totalErrors int64
	for _, cats := range r.errorCategories {
		for _, n := range cats {
			totalErrors += n
		}
	}
	return StatusJSON{
		Version:       version,
		OK:            true,
		UptimeSeconds: int64(time.Since(r.uptimeStart).Seconds()),
		Proxies:       proxyHealth,
		Metrics: StatusMetrics{
			TotalRequests: r.requestsTotal.Load(),
			TotalErrors:   totalErrors,
		},
		Status: detail,
	}
}
