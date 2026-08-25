// Package runtime holds the swappable live snapshot of the assembled gateway.
//
// The gateway supports a hot config reload (the /_dashboard "reload" action,
// and the anticipated /admin/reload endpoint): re-reading config.yaml and
// rebuilding the key pools, proxy health, breakers, routing table, forwarder,
// and validator while the server keeps serving. To make that swap safe under
// concurrent requests, the server and the dashboard read the live objects
// through a single atomic pointer to a Live value rather than chasing many
// independent fields.
//
// runtime is a leaf-aggregator package: it imports only the leaf and near-leaf
// packages whose concrete types make up a snapshot (config, model, forward,
// keypool, proxy, breaker, metrics, auth). It holds no behavior beyond
// loading/storing the pointer, so neither the server nor the dashboard grow
// a dependency on the composition root (internal/app). app builds Live values;
// the server and dashboard consume them.
package runtime

import (
	"sync/atomic"

	"github.com/not-lucky/conflux/internal/auth"
	"github.com/not-lucky/conflux/internal/breaker"
	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/forward"
	"github.com/not-lucky/conflux/internal/keypool"
	"github.com/not-lucky/conflux/internal/metrics"
	"github.com/not-lucky/conflux/internal/model"
	"github.com/not-lucky/conflux/internal/proxy"
)

// Live is an immutable snapshot of the gateway's swappable runtime state.
// Every field is constructed together by the composition root on a (re)build
// and then published atomically via Store.Store, so a reader that does
// live := store.Load() sees a consistent set.
//
// The non-swappable observers (metrics.Registry, trace.Tracer,
// ratelimit.Limiter, persist.Store) are deliberately NOT in Live: they persist
// across a reload so counters, traces, and rate-limit windows are not reset.
type Live struct {
	Config      *config.Config
	Registry    *model.Registry
	Forwarder   *forward.Forwarder
	Validator   *auth.Validator
	Pools       map[string]*keypool.Pool
	Breakers    map[string]*breaker.Breaker
	Health      *proxy.Health
	ProxyHealth func() map[string]metrics.ProxyHealth
}

// Store holds the current Live snapshot behind an atomic pointer. Safe for
// concurrent Load/Store. The zero value is a usable store whose Load returns
// nil until the first Store.
type Store struct {
	p atomic.Pointer[Live]
}

// Load returns the current Live snapshot, or nil before the first Store.
func (s *Store) Load() *Live { return s.p.Load() }

// Store atomically publishes a new Live snapshot. Callers must build the
// Live value fully before calling Store so readers never observe a
// half-built snapshot.
func (s *Store) Store(l *Live) { s.p.Store(l) }
