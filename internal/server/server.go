// Package server implements the HTTP lifecycle: client-key extraction,
// global auth, model extraction, provider matching, rate limiting, forwarding,
// reserved paths, diagnostic header injection, and graceful shutdown.
//
// server imports forward, the deep Do seam, plus auth, config, forward,
// metrics, model, ratelimit, redact, runtime, stream, and trace. It does not
// import keypool, proxy, classify; those are hidden behind forward.Do and the
// proxyHealth closure seam wired by the app (and published via runtime.Live).
//
// The swappable runtime (config, registry, forwarder, validator, pools,
// breakers, proxy health) is read through runtime.Store so a hot config
// reload (see internal/app.Reload and the /_dashboard reload action) is
// visible to in-flight handlers without restarting the process. The stable
// observers (metrics, tracer, limiter) are held directly because they
// persist across a reload.
//
// The package is split by concern across files in this directory:
//   - server.go: the Server struct, New, the mux (Handler), and Serve.
//   - proxy.go: the proxied-request lifecycle (handleProxy) and its helpers,
//     including the unified finishError/finishSuccess terminal paths.
//   - models.go: the /v1/models and /models endpoints and the model-list
//     contract helpers.
//   - status.go: /_status and /metrics.
package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/not-lucky/conflux/internal/auth"
	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/forward"
	"github.com/not-lucky/conflux/internal/metrics"
	"github.com/not-lucky/conflux/internal/model"
	"github.com/not-lucky/conflux/internal/ratelimit"
	"github.com/not-lucky/conflux/internal/runtime"
	"github.com/not-lucky/conflux/internal/trace"
)

// Server owns the http.Handler and dispatches reserved and proxied paths.
type Server struct {
	// live is the swappable runtime snapshot. Handlers load it once per
	// request and read config/registry/forwarder/validator/proxyHealth from
	// it, so a config reload is picked up on the next request.
	live *runtime.Store

	// Stable observers that persist across a reload: the metrics registry
	// keeps its counters, the tracer keeps its trace root, and the rate
	// limiter keeps its per-key windows.
	Limiter *ratelimit.Limiter
	Metrics *metrics.Registry
	Tracer  *trace.Tracer

	// Dashboard, when non-nil, is the management console mounted at
	// /_dashboard/. It is wired by the composition root after New; tests leave
	// it nil so /_dashboard is not mounted (and falls through to handleProxy).
	Dashboard http.Handler
}

// New builds a Server from the assembled app pieces. proxyHealth is a closure
// seam supplied by the app so server stays decoupled from proxy: proxyHealth
// reads the proxy health snapshot. It may be nil, in which case /_status
// reports all proxies as healthy.
//
// New keeps its signature stable so existing tests (and the composition root)
// continue to build a Server from concrete pieces; it wraps them into a
// runtime.Live snapshot internally. To swap the runtime at runtime (a config
// reload), call SetLive.
func New(cfg *config.Config, reg *model.Registry, fwd *forward.Forwarder, lim *ratelimit.Limiter, mreg *metrics.Registry, tr *trace.Tracer, proxyHealth func() map[string]metrics.ProxyHealth) *Server {
	store := &runtime.Store{}
	store.Store(&runtime.Live{
		Config:      cfg,
		Registry:    reg,
		Forwarder:   fwd,
		Validator:   auth.NewValidator(cfg.Auth.ClientKeys),
		ProxyHealth: proxyHealth,
	})
	return &Server{live: store, Limiter: lim, Metrics: mreg, Tracer: tr}
}

// SetLive atomically swaps the swappable runtime snapshot, used by the
// composition root after a config reload. The stable observers (Limiter,
// Metrics, Tracer) are unchanged.
func (s *Server) SetLive(l *runtime.Live) { s.live.Store(l) }

// liveSnapshot is a convenience for handlers to grab the current snapshot.
func (s *Server) liveSnapshot() *runtime.Live { return s.live.Load() }

// Handler returns the http.Handler implementing the request lifecycle.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/_status", s.handleStatus)
	mux.HandleFunc("/v1/models", s.handleModelsList)
	mux.HandleFunc("/v1/models/", s.handleModelDetail)
	mux.HandleFunc("/models", s.handleModelsList)
	mux.HandleFunc("/models/", s.handleModelDetail)
	if s.Dashboard != nil {
		mux.Handle("/_dashboard/", s.Dashboard)
	}
	mux.HandleFunc("/", s.handleProxy)
	return mux
}

// Serve runs the HTTP server until ctx is done, then gracefully shuts down.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
