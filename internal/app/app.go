// Package app is the composition root. It builds the live runtime (key
// pools, proxy health, breakers, forwarder, metrics, tracing, persistence)
// from a resolved config and owns the HTTP server lifecycle.
//
// app is the only package that imports every other internal package and
// wires concrete adapters together. Nothing imports app except cmd/conflux.
//
// The runtime provider map adapts config, keypool, proxy, and breaker to the
// forward.ProviderHandle interface, so the forwarder stays decoupled from
// those packages: forward only sees the interface.
package app

import (
	"errors"
	"time"

	"github.com/not-lucky/conflux/internal/auth"
	"github.com/not-lucky/conflux/internal/breaker"
	"github.com/not-lucky/conflux/internal/clock"
	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/forward"
	"github.com/not-lucky/conflux/internal/keypool"
	"github.com/not-lucky/conflux/internal/metrics"
	"github.com/not-lucky/conflux/internal/model"
	"github.com/not-lucky/conflux/internal/persist"
	"github.com/not-lucky/conflux/internal/proxy"
	"github.com/not-lucky/conflux/internal/ratelimit"
	"github.com/not-lucky/conflux/internal/redact"
	"github.com/not-lucky/conflux/internal/runtime"
	"github.com/not-lucky/conflux/internal/trace"
)

// App is the assembled gateway.
type App struct {
	Config    *config.Config
	Registry  *model.Registry
	Health    *proxy.Health
	Pools     map[string]*keypool.Pool
	Breakers  map[string]*breaker.Breaker
	Limiter   *ratelimit.Limiter
	Metrics   *metrics.Registry
	Tracer    *trace.Tracer
	Store     *persist.Store
	Forwarder *forward.Forwarder

	// Live is the swappable runtime snapshot the server and dashboard read
	// through. It is published on Build and again on a successful Reload so a
	// hot config swap is visible to in-flight handlers without restarting the
	// process. The non-swappable observers (Metrics, Tracer, Limiter, Store)
	// live directly on App and persist across a reload.
	Live *runtime.Store
}

// Build assembles the runtime from a resolved config.
func Build(cfg *config.Config) (*App, error) {
	clk := clock.RealClock{}
	pclock := clock.RealClock{}

	reg := metrics.New(time.Now())
	tr := trace.New("./logs", trace.ParseLevel(cfg.Logging.Level), cfg.Logging.MaxDirs)
	lim := ratelimit.New(clk)

	store := persist.New("")
	if cfg.Persistence != nil && cfg.Persistence.Path != "" {
		store = persist.New(cfg.Persistence.Path)
	}

	// On a cold start, restore from the persisted state file. On a reload,
	// the caller supplies the freshest live state instead (see Reload).
	st, _ := store.Load()
	rt, err := buildLive(cfg, st, store, reg, clk, pclock)
	if err != nil {
		return nil, err
	}

	app := &App{
		Config: cfg, Registry: rt.Registry, Health: rt.Health,
		Pools: rt.Pools, Breakers: rt.Breakers, Limiter: lim,
		Metrics: reg, Tracer: tr, Store: store,
		Forwarder: rt.Forwarder, Live: rt.Store,
	}
	return app, nil
}

// builtRuntime is the result of building a fresh Live snapshot: the
// constituent objects plus the published Store. Callers (Build, Reload) copy
// the pieces they need onto App fields and hand the Store to the server and
// dashboard.
type builtRuntime struct {
	Registry  *model.Registry
	Health    *proxy.Health
	Pools     map[string]*keypool.Pool
	Breakers  map[string]*breaker.Breaker
	Forwarder *forward.Forwarder
	Store     *runtime.Store
}

// buildLive constructs the swappable runtime from a resolved config and a
// prior state snapshot, reusing the stable store and metrics so counters and
// persistence survive a reload. It builds the routing table, per-provider key
// pools and breakers, the global proxy health, the forwarder with its provider
// handles, and publishes them all into a fresh runtime.Store. On a reload the
// priorState is the freshest live state (snapshotted from the old pools), so
// key exhaustion and proxy trips carry over; on a cold start it is the state
// loaded from the persisted file (possibly empty).
func buildLive(cfg *config.Config, priorState persist.State, store *persist.Store, mreg *metrics.Registry, clk, pclock clock.Clock) (*builtRuntime, error) {
	// Build the model routing table.
	provModels := make([]model.Provider, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		entries := make([]model.Entry, 0, len(p.Models))
		for _, me := range p.Models {
			var k model.Kind
			switch me.Kind {
			case config.ModelExact:
				k = model.Exact
			case config.ModelPrefix:
				k = model.Prefix
			case config.ModelCatchAll:
				k = model.CatchAll
			}
			entries = append(entries, model.Entry{Kind: k, Literal: me.Literal})
		}
		provModels = append(provModels, model.Provider{Name: p.Name, Models: entries})
	}
	registry := model.NewRegistry(provModels)

	// Build a per-provider key pool and circuit breaker for each provider.
	pools := map[string]*keypool.Pool{}
	breakers := map[string]*breaker.Breaker{}
	for _, p := range cfg.Providers {
		keys := make([]keypool.Key, 0, len(p.Keys))
		for _, k := range p.Keys {
			keys = append(keys, keypool.Key{Value: k.Value, Proxy: k.Proxy})
		}
		pools[p.Name] = keypool.New(keypool.Spec{
			Keys:               keys,
			Mode:               p.KeySelection.Mode,
			RequestsPerKey:     p.KeySelection.RequestsPerKey,
			ActiveWindow:       p.ActiveWindow,
			MaxErrors:          p.MaxErrors,
			Cooldown:           p.Cooldown,
			RetireOnExhaustion: p.RetireOnExhaustion,
		}, clk)
		breakers[p.Name] = breaker.New(p.Upstream5xxThreshold, p.Upstream5xxCooldown, nil)
	}

	health := proxy.NewHealth(pclock)
	restoreState(cfg, pools, health, priorState)

	handles := buildHandles(cfg, pools, breakers, health, store, mreg)
	lookup := &mapLookup{handles: handles}
	fwd := &forward.Forwarder{Doer: newHTTPDoer(), Providers: lookup}

	liveStore := &runtime.Store{}
	liveStore.Store(&runtime.Live{
		Config:    cfg,
		Registry:  registry,
		Forwarder: fwd,
		Validator: auth.NewValidator(cfg.Auth.ClientKeys),
		Pools:     pools,
		Breakers:  breakers,
		Health:    health,
		ProxyHealth: func() map[string]metrics.ProxyHealth {
			return proxyHealthFromEntries(health.Snapshot(cfg.ProxyURLs()))
		},
	})

	// Push the initial gauges from the restored state so /metrics is correct
	// immediately after a (re)build, not only after the first live transition.
	saver := &stateSaver{cfg: cfg, pools: pools, health: health, store: store, metrics: mreg}
	saver.pushGauges()

	return &builtRuntime{
		Registry: registry, Health: health, Pools: pools, Breakers: breakers,
		Forwarder: fwd, Store: liveStore,
	}, nil
}

// Reload re-reads the config at cfgPath, rebuilds the swappable runtime while
// preserving the current in-memory key/proxy state, and atomically publishes
// the new Live snapshot. The stable observers (Metrics, Tracer, Limiter,
// persist Store) are kept, so counters, traces, and rate-limit windows are
// not reset across a reload. On a config error the old snapshot is left in
// place and the error is returned unchanged.
//
// Reload is safe to call concurrently with request serving: in-flight
// requests continue against the old Live (its handles keep the old pools
// alive for GC), and new requests pick up the new Live on their next
// store.Load().
func (a *App) Reload(cfgPath string) error {
	newCfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	prior := a.Live.Load()
	var priorState persist.State
	if prior != nil {
		priorState = snapshotState(prior.Config, prior.Pools, prior.Health)
	}
	rt, err := buildLive(newCfg, priorState, a.Store, a.Metrics, clock.RealClock{}, clock.RealClock{})
	if err != nil {
		return err
	}
	// Publish the new snapshot, then mirror the new pieces onto App fields so
	// legacy readers (and tests) that go through App see the refreshed state.
	a.Live.Store(rt.Store.Load())
	a.Config = rt.Store.Load().Config
	a.Registry = rt.Registry
	a.Health = rt.Health
	a.Pools = rt.Pools
	a.Breakers = rt.Breakers
	a.Forwarder = rt.Forwarder
	return nil
}

// ProxyHealthSnapshot returns the health snapshot as a map keyed by the
// credential-stripped URL, converting proxy.SnapshotEntry into
// metrics.ProxyHealth so server stays decoupled from proxy. DeadUntil is
// converted to a unix-ms pointer, nil when not tripped.
func (a *App) ProxyHealthSnapshot() map[string]metrics.ProxyHealth {
	live := a.Live.Load()
	if live == nil {
		return nil
	}
	entries := live.Health.Snapshot(live.Config.ProxyURLs())
	return proxyHealthFromEntries(entries)
}

// proxyHealthFromEntries converts proxy snapshot entries into the
// /_status ProxyHealth map, shared by the live snapshot and the app-level
// convenience method.
func proxyHealthFromEntries(entries []proxy.SnapshotEntry) map[string]metrics.ProxyHealth {
	out := make(map[string]metrics.ProxyHealth, len(entries))
	for _, e := range entries {
		var du *int64
		if !e.DeadUntil.IsZero() {
			ms := e.DeadUntil.UnixMilli()
			du = &ms
		}
		out[redact.URL(e.URL)] = metrics.ProxyHealth{
			Healthy:           e.Healthy,
			ConsecutiveErrors: e.ConsecutiveErrors,
			DeadUntil:         du,
			LastError:         e.LastError,
		}
	}
	return out
}

// restoreState applies a persisted State to the pools and proxy health by
// matching provider key hashes. Keys are matched by SHA256 hash so a config
// change (key reordered, removed, or value changed) skips the stale record.
// Proxy dead-until timestamps are restored via Health.Restore. Best-effort:
// a config-changed key whose hash no longer matches is skipped silently.
func restoreState(cfg *config.Config, pools map[string]*keypool.Pool, health *proxy.Health, st persist.State) {
	// Restore keys: group records by provider into one per-provider snapshot,
	// match each by hash, then call pool.Restore once per provider. Pool.Restore
	// overwrites all keys, so accumulating into a single snapshot per provider
	// avoids the last record zeroing the earlier ones.
	type provSnap struct {
		keys []keypool.KeyState
	}
	snaps := map[string]*provSnap{}
	for _, r := range st.Keys {
		p, ok := cfg.ProviderByName(r.Provider)
		if !ok {
			continue
		}
		if _, ok := pools[r.Provider]; !ok {
			continue
		}
		ps, exists := snaps[r.Provider]
		if !exists {
			ps = &provSnap{keys: make([]keypool.KeyState, len(p.Keys))}
			snaps[r.Provider] = ps
		}
		idx := -1
		for i, k := range p.Keys {
			if persist.HashKey(k.Value) == r.KeyHash {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		ps.keys[idx] = keypool.KeyState{
			ConsecutiveErrors: r.ConsecutiveErrors,
			ExhaustedAt:       derefTime(r.ExhaustedAt),
			Retired:           r.Retired,
			RetiredAt:         derefTime(r.RetiredAt),
		}
	}
	for name, ps := range snaps {
		pools[name].Restore(keypool.Snapshot{Keys: ps.keys})
	}

	// Restore proxy health.
	if len(st.Proxies) == 0 {
		return
	}
	now := time.Now()
	entries := make([]proxy.SnapshotEntry, 0, len(st.Proxies))
	for _, pr := range st.Proxies {
		du := derefTime(pr.DeadUntil)
		entries = append(entries, proxy.SnapshotEntry{
			URL:               pr.URL,
			ConsecutiveErrors: pr.ConsecutiveErrors,
			DeadUntil:         du,
			Healthy:           du.IsZero() || du.After(now),
		})
	}
	health.Restore(entries)
}

// derefTime returns the time, or zero when the pointer is nil.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ptrIfSet returns nil when t is zero, else &t, so omitempty JSON tags work.
func ptrIfSet(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// snapshotState builds a persist.State from the live pools and proxy health.
// It is a pure function with no I/O. Keys are ordered by config provider
// order and declaration index; proxies are deduplicated by URL.
func snapshotState(cfg *config.Config, pools map[string]*keypool.Pool, health *proxy.Health) persist.State {
	var st persist.State

	// Keys.
	for _, p := range cfg.Providers {
		pool, ok := pools[p.Name]
		if !ok {
			continue
		}
		snap := pool.Snapshot()
		for i, ks := range snap.Keys {
			if i >= len(p.Keys) {
				break
			}
			var reason string
			switch {
			case ks.Retired:
				reason = "retired"
			case !ks.ExhaustedAt.IsZero():
				reason = "exhausted"
			}
			st.Keys = append(st.Keys, persist.KeyRecord{
				Provider:          p.Name,
				KeyHash:           persist.HashKey(p.Keys[i].Value),
				ConsecutiveErrors: ks.ConsecutiveErrors,
				ExhaustedAt:       ptrIfSet(ks.ExhaustedAt),
				Retired:           ks.Retired,
				RetiredAt:         ptrIfSet(ks.RetiredAt),
				Reason:            reason,
			})
		}
	}

	// Proxies: collect the full set of URLs, deduplicated preserving order.
	for _, e := range health.Snapshot(cfg.ProxyURLs()) {
		st.Proxies = append(st.Proxies, persist.ProxyRecord{
			URL:               e.URL,
			ConsecutiveErrors: e.ConsecutiveErrors,
			DeadUntil:         ptrIfSet(e.DeadUntil),
		})
	}

	return st
}

// stateSaver captures the collaborators needed to persist a snapshot of the
// runtime state and push gauge metrics on state change. It is shared across
// all provider handles so every transition goes through the same saver.
type stateSaver struct {
	cfg     *config.Config
	pools   map[string]*keypool.Pool
	health  *proxy.Health
	store   *persist.Store
	metrics *metrics.Registry
}

// mark persists the current state and pushes the gauge metrics. It is the
// single entry point for state transitions. Set is a no-op when the store
// path is empty (persistence disabled).
func (s *stateSaver) mark() {
	s.store.Set(snapshotState(s.cfg, s.pools, s.health))
	s.pushGauges()
}

// flushNow flushes the persisted state to disk immediately. Used on
// retirement-grade changes that must survive a crash.
func (s *stateSaver) flushNow() {
	s.store.FlushImmediately()
}

// pushGauges sets the key-state and proxy-health gauges in the metrics
// registry from the live pools and health so /metrics is correct regardless
// of whether /_status has been hit.
func (s *stateSaver) pushGauges() {
	for _, p := range s.cfg.Providers {
		pool, ok := s.pools[p.Name]
		if !ok {
			continue
		}
		c := pool.Counts()
		s.metrics.SetKeysGauge(p.Name, int64(c.Active), int64(c.Standby), int64(c.Exhausted), int64(c.Retired))
	}
	for _, e := range s.health.Snapshot(s.cfg.ProxyURLs()) {
		s.metrics.SetProxyHealthy(redact.URL(e.URL), e.Healthy)
	}
}

// mapLookup is a ProviderLookup backed by a pre-built map of provider
// handles. It replaces runtimeLookup: no closures are allocated per request.
type mapLookup struct {
	handles map[string]forward.ProviderHandle
}

func (l *mapLookup) Lookup(name string) (forward.ProviderHandle, error) {
	h, ok := l.handles[name]
	if !ok {
		return nil, errUnknownProvider
	}
	return h, nil
}

// providerHandle is the concrete per-provider handle built once at Build
// time. It holds the concrete collaborators (keypool, breaker, proxy health)
// and resolved policy, and implements forward.ProviderHandle.
type providerHandle struct {
	name   string
	pool   *keypool.Pool
	brk    *breaker.Breaker
	health *proxy.Health
	ppc    *proxy.PoolConfig // provider-scoped proxy pool, or nil
	gpc    *proxy.PoolConfig // global proxy pool
	policy forward.ProviderPolicy
	saver  *stateSaver
}

// buildHandles constructs one providerHandle per provider at (re)build time.
// It takes the concrete collaborators directly rather than *App so it can run
// during a Reload against freshly built pools/breakers/health while reusing the
// stable store and metrics.
func buildHandles(cfg *config.Config, pools map[string]*keypool.Pool, breakers map[string]*breaker.Breaker, health *proxy.Health, store *persist.Store, mreg *metrics.Registry) map[string]forward.ProviderHandle {
	saver := &stateSaver{
		cfg:     cfg,
		pools:   pools,
		health:  health,
		store:   store,
		metrics: mreg,
	}
	handles := make(map[string]forward.ProviderHandle, len(cfg.Providers))

	gpc := &proxy.PoolConfig{
		URLs: cfg.Proxies.URLs, RotateInterval: cfg.Proxies.RotateInterval,
		MaxErrors: cfg.Proxies.MaxErrors, Cooldown: cfg.Proxies.Cooldown,
	}

	for _, p := range cfg.Providers {
		pool := pools[p.Name]
		brk := breakers[p.Name]

		aw := p.ActiveWindow
		if aw == 0 {
			aw = len(p.Keys)
		}

		var ppc *proxy.PoolConfig
		if p.Proxies != nil {
			ppc = &proxy.PoolConfig{
				URLs: p.Proxies.URLs, RotateInterval: p.Proxies.RotateInterval,
				MaxErrors: p.Proxies.MaxErrors, Cooldown: p.Proxies.Cooldown,
			}
		}

		handles[p.Name] = &providerHandle{
			name:   p.Name,
			pool:   pool,
			brk:    brk,
			health: health,
			ppc:    ppc,
			gpc:    gpc,
			policy: forward.ProviderPolicy{
				Name:              p.Name,
				BaseURL:           p.BaseURL,
				MaxAttempts:       p.RetryMaxAttempts,
				ActiveWindowSize:  aw,
				MaxStreamRetries:  p.MaxStreamRetries,
				RequestTimeout:    p.RequestTimeout,
				StreamIdleTimeout: p.StreamIdleTimeout,
				KeepaliveInterval: p.StreamKeepaliveInterval,
				RequestDeadline:   p.RequestDeadline,
				FallbackModels:    p.FallbackModels,
			},
			saver: saver,
		}
	}
	return handles
}

func (h *providerHandle) Policy() forward.ProviderPolicy { return h.policy }

func (h *providerHandle) Select() (forward.Selection, error) {
	sel, err := h.pool.Select()
	if err != nil {
		return forward.Selection{}, err
	}
	return forward.Selection{
		Key: sel.Key.Value, KeyNumber: sel.KeyNumber, SlotIndex: sel.SlotIndex, Proxy: sel.Key.Proxy, CycleCount: sel.CycleCount,
	}, nil
}

func (h *providerHandle) RecordSuccess(keyNumber int) {
	h.pool.RecordSuccess(keyNumber)
}

func (h *providerHandle) RecordError(keyNumber int) forward.RecordResult {
	r := h.pool.RecordError(keyNumber)
	// Persist only on a threshold transition (exhaustion or retirement).
	// Sub-threshold increments are cheap and batched later.
	if r.Exhausted || r.Retired {
		h.saver.mark()
	}
	return forward.RecordResult{Exhausted: r.Exhausted, Retired: r.Retired}
}

func (h *providerHandle) MarkExhausted(keyNumber int) {
	h.pool.MarkExhausted(keyNumber)
	// Retirement-grade change: persist and flush immediately.
	h.saver.mark()
	h.saver.flushNow()
}

func (h *providerHandle) ResolveProxy(slotIndex, cycleCount int, sel forward.Selection) forward.ProxySelection {
	r := proxy.Resolver{
		Health: h.health, ProviderPool: h.ppc, GlobalPool: h.gpc,
		CycleCount: cycleCount,
	}
	r.Inline = sel.Proxy
	ps := r.Resolve(slotIndex)
	return forward.ProxySelection{URL: ps.URL, Number: ps.Number, Direct: ps.Direct}
}

func (h *providerHandle) RecordProxyError(url string) {
	cfg := h.effectiveProxyCfg()
	h.health.RecordError(url, cfg.MaxErrors, cfg.Cooldown)
	h.saver.mark()
}

func (h *providerHandle) SetProxyLastError(url, msg string) {
	h.health.SetLastError(url, msg)
	h.saver.mark()
}

func (h *providerHandle) RecordProxySuccess(url string) {
	h.health.RecordSuccess(url)
	h.saver.mark()
}

func (h *providerHandle) BreakerOpen() bool  { return h.brk.Open() }
func (h *providerHandle) BreakerOn5xx() bool { return h.brk.On5xx() }
func (h *providerHandle) BreakerOn2xx()      { h.brk.On2xx() }

// effectiveProxyCfg returns the provider pool when it has URLs, else the
// global pool.
func (h *providerHandle) effectiveProxyCfg() proxy.PoolConfig {
	if h.ppc != nil && len(h.ppc.URLs) > 0 {
		return *h.ppc
	}
	return *h.gpc
}

// errUnknownProvider is returned by Lookup for an unknown provider name.
var errUnknownProvider = errors.New("unknown provider")
