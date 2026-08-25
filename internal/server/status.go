package server

import (
	"encoding/json"
	"net/http"

	"github.com/not-lucky/conflux/internal/metrics"
	"github.com/not-lucky/conflux/internal/redact"
	"github.com/not-lucky/conflux/internal/version"
)

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.Metrics.WritePrometheus(w)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	l := s.liveSnapshot()
	cfg := l.Config
	clientKeys := make([]string, len(cfg.Auth.ClientKeys))
	for i, k := range cfg.Auth.ClientKeys {
		clientKeys[i] = redact.Key(k)
	}
	globalProxies := make([]string, len(cfg.Proxies.URLs))
	for i, u := range cfg.Proxies.URLs {
		globalProxies[i] = redact.URL(u)
	}
	providers := map[string]any{}
	for _, p := range cfg.Providers {
		providers[p.Name] = map[string]any{
			"baseUrl":              p.BaseURL,
			"models":               modelStrings(p.Models),
			"maxConsecutiveErrors": p.MaxErrors,
			"cooldownMs":           p.Cooldown.Milliseconds(),
			"keyStrategy":          p.KeySelection.Mode,
			"requestsPerKey":       p.KeySelection.RequestsPerKey,
			"retireKeys":           p.RetireOnExhaustion,
			"activeKeys":           effectiveActiveKeys(p),
			"retryMaxAttempts":     p.RetryMaxAttempts,
		}
	}
	detail := metrics.StatusDetail{
		GlobalProxies: globalProxies,
		ClientKeys:    clientKeys,
		Providers:     providers,
	}
	// The app pushes key-pool gauges and proxy health gauges on state
	// change; /_status only reads metrics.

	ph := map[string]metrics.ProxyHealth{}
	if l.ProxyHealth != nil {
		ph = l.ProxyHealth()
	} else {
		for _, u := range cfg.ProxyURLs() {
			ph[redact.URL(u)] = metrics.ProxyHealth{Healthy: true}
		}
	}
	st := s.Metrics.Status(version.Version, detail, ph)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(st)
}
