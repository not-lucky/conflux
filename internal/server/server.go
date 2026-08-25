// Package server implements the HTTP lifecycle: client-key extraction,
// global auth, model extraction, provider matching, rate limiting, forwarding,
// reserved paths, diagnostic header injection, and graceful shutdown.
//
// server imports forward, the deep Do seam, plus auth, config, forward, metrics,
// model, ratelimit, redact, stream, and trace. It does not import keypool,
// proxy, classify; those are hidden behind forward.Do and the proxyHealth
// closure seam wired by the app.
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
	"github.com/not-lucky/conflux/internal/trace"
)

// Server owns the http.Handler and dispatches reserved and proxied paths.
type Server struct {
	Config    *config.Config
	Registry  *model.Registry
	Forwarder *forward.Forwarder
	Validator *auth.Validator
	Limiter   *ratelimit.Limiter
	Metrics   *metrics.Registry
	Tracer    *trace.Tracer
	// proxyHealth returns the real health snapshot as a map keyed by the
	// credential-stripped URL. Wired by the app from proxy.Health.Snapshot so
	// server does not import proxy.
	proxyHealth func() map[string]metrics.ProxyHealth
}

// New builds a Server from the assembled app pieces. proxyHealth is a closure
// seam supplied by the app so server stays decoupled from proxy: proxyHealth
// reads the proxy health snapshot. It may be nil, in which case /_status
// reports all proxies as healthy.
func New(cfg *config.Config, reg *model.Registry, fwd *forward.Forwarder, lim *ratelimit.Limiter, mreg *metrics.Registry, tr *trace.Tracer, proxyHealth func() map[string]metrics.ProxyHealth) *Server {
	return &Server{
		Config: cfg, Registry: reg, Forwarder: fwd,
		Validator: auth.NewValidator(cfg.Auth.ClientKeys),
		Limiter:   lim, Metrics: mreg, Tracer: tr,
		proxyHealth: proxyHealth,
	}
}

// Handler returns the http.Handler implementing the request lifecycle.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/_status", s.handleStatus)
	mux.HandleFunc("/v1/models", s.handleModelsList)
	mux.HandleFunc("/v1/models/", s.handleModelDetail)
	mux.HandleFunc("/models", s.handleModelsList)
	mux.HandleFunc("/models/", s.handleModelDetail)
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
