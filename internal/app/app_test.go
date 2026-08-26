package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/not-lucky/conflux/internal/clock"
	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/keypool"
	"github.com/not-lucky/conflux/internal/metrics"
	"github.com/not-lucky/conflux/internal/persist"
	"github.com/not-lucky/conflux/internal/proxy"
)

// TestBuildSmoke loads the repository sample config (config.example.yaml,
// the committed fixture) and builds the runtime to catch wiring mismatches
// between config, app, and the forwarder. It deliberately does NOT read the
// operator's gitignored config.yaml, which may be customized to different
// providers; the sample fixture is the stable, version-controlled contract.
func TestBuildSmoke(t *testing.T) {
	cfg, err := config.Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("Load config.example.yaml: %v", err)
	}
	a, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if a.Registry == nil || a.Forwarder == nil {
		t.Fatal("nil collaborators after Build")
	}
	// Verify the provider set.
	if len(a.Pools) != len(cfg.Providers) {
		t.Errorf("pools = %d, want %d", len(a.Pools), len(cfg.Providers))
	}
	if len(a.Breakers) != len(cfg.Providers) {
		t.Errorf("breakers = %d, want %d", len(a.Breakers), len(cfg.Providers))
	}
	// Verify routing: gpt-4o maps to openai, claude-3-5-sonnet maps to
	// anthropic, and an unknown model maps to the catch-all provider.
	for _, c := range []struct{ model, want string }{
		{"gpt-4o", "openai"},
		{"gpt-4-turbo", "openai"}, // prefix gpt-4*
		{"claude-3-5-sonnet", "anthropic"},
		{"claude-3-opus", "anthropic"}, // prefix claude-3*
		{"some-other-model", "catchall"},
	} {
		got, ok := a.Registry.Match(c.model)
		if !ok || got != c.want {
			t.Errorf("Match(%q) = (%q,%v), want (%q,true)", c.model, got, ok, c.want)
		}
	}
}

// minimalConfig builds a config with one provider, two keys, and persistence
// at the given path. MaxErrors is 2 so RecordError exhausts quickly.
func minimalConfig(persistPath string) *config.Config {
	return &config.Config{
		Providers: []config.Provider{{
			Name:    "test",
			BaseURL: "http://upstream.test",
			Models: []config.ModelEntry{{
				Kind: config.ModelCatchAll, Literal: "*",
			}},
			Keys: []config.Key{
				{Value: "key-1"},
				{Value: "key-2"},
			},
			PolicyFields: config.PolicyFields{
				MaxErrors:    2,
				Cooldown:     30 * time.Second,
				ActiveWindow: 0, // all healthy
			},
		}},
		Persistence: &config.PersistenceConfig{Path: persistPath},
	}
}

// TestPersistRoundTrip builds an app, exhausts key #1, flushes, then builds a
// second app from the same config+path and asserts key #1 is restored as
// exhausted.
func TestPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.yaml")

	cfg := minimalConfig(path)

	// First build.
	a1, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}

	// Exhaust key #1 by recording errors to the threshold.
	pool1 := a1.Pools["test"]
	pool1.RecordError(1) // 1 error
	pool1.RecordError(1) // 2 errors -> exhausted (MaxErrors=2, RetireOnExhaustion=false)

	// Verify it actually exhausted.
	snap := pool1.Snapshot()
	if snap.Keys[0].ExhaustedAt.IsZero() {
		t.Fatalf("key #1 should be exhausted, snapshot = %+v", snap.Keys[0])
	}

	// Flush to disk.
	a1.Store.Set(snapshotState(a1.Config, a1.Pools, a1.Health))
	a1.Store.FlushImmediately()

	// Second build with the same config + path.
	a2, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}

	// Key #1 should be restored as exhausted.
	pool2 := a2.Pools["test"]
	snap2 := pool2.Snapshot()
	if snap2.Keys[0].ExhaustedAt.IsZero() {
		t.Errorf("key #1 not restored exhausted: %+v", snap2.Keys[0])
	}
	if snap2.Keys[0].ConsecutiveErrors != 2 {
		t.Errorf("key #1 ConsecutiveErrors = %d, want 2", snap2.Keys[0].ConsecutiveErrors)
	}
	// Key #2 should be clean.
	if !snap2.Keys[1].ExhaustedAt.IsZero() {
		t.Errorf("key #2 should not be exhausted: %+v", snap2.Keys[1])
	}

	// Counts should reflect one exhausted key.
	counts := pool2.Counts()
	if counts.Exhausted < 1 {
		t.Errorf("counts.Exhausted = %d, want >= 1", counts.Exhausted)
	}
}

// TestPersistRoundTripRetired verifies retirement (RetireOnExhaustion=true) is
// persisted and restored.
func TestPersistRoundTripRetired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.yaml")

	cfg := minimalConfig(path)
	cfg.Providers[0].RetireOnExhaustion = true

	a1, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}

	pool1 := a1.Pools["test"]
	pool1.RecordError(1)
	pool1.RecordError(1) // -> retired

	snap := pool1.Snapshot()
	if !snap.Keys[0].Retired {
		t.Fatalf("key #1 should be retired: %+v", snap.Keys[0])
	}

	a1.Store.Set(snapshotState(a1.Config, a1.Pools, a1.Health))
	a1.Store.FlushImmediately()

	a2, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}

	pool2 := a2.Pools["test"]
	snap2 := pool2.Snapshot()
	if !snap2.Keys[0].Retired {
		t.Errorf("key #1 not restored retired: %+v", snap2.Keys[0])
	}
	if snap2.Keys[0].RetiredAt.IsZero() {
		t.Errorf("key #1 RetiredAt not restored: %+v", snap2.Keys[0])
	}
}

// TestPersistProxyRestore verifies a tripped proxy is persisted and restored.
func TestPersistProxyRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.yaml")
	cfg := minimalConfig(path)
	cfg.Proxies = config.GlobalProxyConfig{
		URLs:      []string{"http://proxy:8080"},
		MaxErrors: 1,
		Cooldown:  60 * time.Second,
	}

	a1, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}

	// Trip the proxy.
	a1.Health.RecordError("http://proxy:8080", 1, 60*time.Second)
	if a1.Health.Healthy("http://proxy:8080") {
		t.Fatal("proxy should be tripped after error")
	}

	a1.Store.Set(snapshotState(a1.Config, a1.Pools, a1.Health))
	a1.Store.FlushImmediately()

	a2, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}

	// Proxy should still be tripped after restore.
	if a2.Health.Healthy("http://proxy:8080") {
		t.Error("proxy not restored as tripped")
	}
	entries := a2.Health.Snapshot([]string{"http://proxy:8080"})
	if len(entries) != 1 || entries[0].ConsecutiveErrors != 1 {
		t.Errorf("proxy ConsecutiveErrors not restored: %+v", entries)
	}
}

// TestSnapshotStateDirect tests snapshotState as a pure function with
// constructed pools and health, verifying key and proxy records.
func TestSnapshotStateDirect(t *testing.T) {
	clk := clock.RealClock{}
	pclk := clock.RealClock{}

	cfg := &config.Config{
		Proxies: config.GlobalProxyConfig{
			URLs: []string{"http://global:8080"}, MaxErrors: 3, Cooldown: 30 * time.Second,
		},
		Providers: []config.Provider{{
			Name:   "p1",
			Models: []config.ModelEntry{{Kind: config.ModelCatchAll, Literal: "*"}},
			Keys:   []config.Key{{Value: "alpha"}, {Value: "beta"}},
			Proxies: &config.ProviderProxyConfig{
				URLs: []string{"http://prov:8080"}, MaxErrors: 3, Cooldown: 30 * time.Second,
			},
		}},
	}

	pools := map[string]*keypool.Pool{}
	pools["p1"] = keypool.New(keypool.Spec{
		Keys:               []keypool.Key{{Value: "alpha"}, {Value: "beta"}},
		MaxErrors:          2,
		Cooldown:           30 * time.Second,
		RetireOnExhaustion: false,
	}, clk)

	// Exhaust key #1.
	pools["p1"].RecordError(1)
	pools["p1"].RecordError(1)

	// Trip the global proxy.
	health := proxy.NewHealth(pclk)
	health.RecordError("http://global:8080", 3, 30*time.Second)

	st := snapshotState(cfg, pools, health)

	if len(st.Keys) != 2 {
		t.Fatalf("keys = %d, want 2", len(st.Keys))
	}
	if st.Keys[0].Provider != "p1" || st.Keys[0].KeyHash != persist.HashKey("alpha") {
		t.Errorf("key[0] = %+v", st.Keys[0])
	}
	if st.Keys[0].ExhaustedAt == nil {
		t.Error("key[0] ExhaustedAt should be set")
	}
	if st.Keys[0].Reason != "exhausted" {
		t.Errorf("key[0] Reason = %q, want \"exhausted\"", st.Keys[0].Reason)
	}
	if st.Keys[1].ExhaustedAt != nil {
		t.Errorf("key[1] should be clean: %+v", st.Keys[1])
	}
	if st.Keys[1].Reason != "" {
		t.Errorf("key[1] Reason = %q, want \"\"", st.Keys[1].Reason)
	}

	// Proxy records should include global, provider, and inline (none here).
	urls := map[string]bool{}
	for _, pr := range st.Proxies {
		urls[pr.URL] = true
	}
	if !urls["http://global:8080"] {
		t.Error("global proxy missing from state")
	}
	if !urls["http://prov:8080"] {
		t.Error("provider proxy missing from state")
	}
}

// TestRestoreStateDirect tests restoreState with a constructed state, verifying
// hash matching and proxy restoration.
func TestRestoreStateDirect(t *testing.T) {
	clk := clock.RealClock{}
	pclk := clock.RealClock{}

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:   "p1",
			Models: []config.ModelEntry{{Kind: config.ModelCatchAll, Literal: "*"}},
			Keys:   []config.Key{{Value: "alpha"}, {Value: "beta"}},
			PolicyFields: config.PolicyFields{
				MaxErrors: 2,
				Cooldown:  30 * time.Second,
			},
		}},
	}

	pools := map[string]*keypool.Pool{}
	pools["p1"] = keypool.New(keypool.Spec{
		Keys:      []keypool.Key{{Value: "alpha"}, {Value: "beta"}},
		MaxErrors: 2,
		Cooldown:  30 * time.Second,
	}, clk)

	health := proxy.NewHealth(pclk)

	ex := time.Now().Add(60 * time.Second) // future: still tripped
	st := persist.State{
		Keys: []persist.KeyRecord{{
			Provider:          "p1",
			KeyHash:           persist.HashKey("alpha"),
			ConsecutiveErrors: 2,
			ExhaustedAt:       &ex,
		}},
		Proxies: []persist.ProxyRecord{{
			URL:               "http://ghost:8080",
			ConsecutiveErrors: 3,
			DeadUntil:         &ex,
		}},
	}

	restoreState(cfg, pools, health, st)

	snap := pools["p1"].Snapshot()
	if snap.Keys[0].ConsecutiveErrors != 2 {
		t.Errorf("key[0] ConsecutiveErrors = %d, want 2", snap.Keys[0].ConsecutiveErrors)
	}
	if !snap.Keys[0].ExhaustedAt.Equal(ex) {
		t.Errorf("key[0] ExhaustedAt = %v, want %v", snap.Keys[0].ExhaustedAt, ex)
	}
	if !snap.Keys[1].ExhaustedAt.IsZero() {
		t.Errorf("key[1] should be clean: %+v", snap.Keys[1])
	}

	if health.Healthy("http://ghost:8080") {
		t.Error("proxy should be restored as tripped")
	}
}

// TestRestoreStateSkipsMismatchedHash verifies that a key whose hash doesn't
// match any current config key is silently skipped.
func TestRestoreStateSkipsMismatchedHash(t *testing.T) {
	clk := clock.RealClock{}

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:   "p1",
			Models: []config.ModelEntry{{Kind: config.ModelCatchAll, Literal: "*"}},
			Keys:   []config.Key{{Value: "alpha"}},
			PolicyFields: config.PolicyFields{
				MaxErrors: 2,
				Cooldown:  30 * time.Second,
			},
		}},
	}

	pools := map[string]*keypool.Pool{}
	pools["p1"] = keypool.New(keypool.Spec{
		Keys:      []keypool.Key{{Value: "alpha"}},
		MaxErrors: 2,
		Cooldown:  30 * time.Second,
	}, clk)

	ex := time.Now()
	st := persist.State{
		Keys: []persist.KeyRecord{{
			Provider:          "p1",
			KeyHash:           "sha256:nonexistent",
			ConsecutiveErrors: 5,
			ExhaustedAt:       &ex,
		}},
	}

	restoreState(cfg, pools, proxy.NewHealth(clock.RealClock{}), st)

	snap := pools["p1"].Snapshot()
	if !snap.Keys[0].ExhaustedAt.IsZero() {
		t.Errorf("mismatched hash should be skipped, got ExhaustedAt = %v", snap.Keys[0].ExhaustedAt)
	}
}

// TestStateSaverPushesGauges verifies that stateSaver.mark() pushes the key-state
// and proxy-health gauges into the metrics registry on a state change, so
// /metrics is correct regardless of whether /_status has been hit. This
// coverage moved from server.handleStatus (which no longer mutates gauges as
// a side effect of a GET) to the app, triggered on state change.
func TestStateSaverPushesGauges(t *testing.T) {
	clk := clock.RealClock{}
	cfg := &config.Config{
		Proxies: config.GlobalProxyConfig{URLs: []string{"http://user:pass@proxy1:8080"}},
		Providers: []config.Provider{{
			Name:   "p1",
			Models: []config.ModelEntry{{Kind: config.ModelCatchAll, Literal: "*"}},
			Keys:   []config.Key{{Value: "alpha"}, {Value: "beta"}},
			PolicyFields: config.PolicyFields{
				MaxErrors: 2,
				Cooldown:  30 * time.Second,
			},
		}},
	}

	pools := map[string]*keypool.Pool{}
	pools["p1"] = keypool.New(keypool.Spec{
		Keys:      []keypool.Key{{Value: "alpha"}, {Value: "beta"}},
		MaxErrors: 2,
		Cooldown:  30 * time.Second,
	}, clk)
	health := proxy.NewHealth(clk)

	reg := metrics.New(time.Now())
	s := &stateSaver{cfg: cfg, pools: pools, health: health, store: persist.New(""), metrics: reg}

	// Exhaust key #1 via the pool, then mark (as the provider handle does on a
	// penalized error). Also trip the proxy breaker.
	pools["p1"].RecordError(1)
	pools["p1"].RecordError(1)                                            // -> exhausted
	health.RecordError("http://user:pass@proxy1:8080", 1, 60*time.Second) // -> tripped
	s.mark()

	var buf strings.Builder
	reg.WritePrometheus(&buf)
	out := buf.String()

	// Key-state gauge reflects one exhausted key.
	if !strings.Contains(out, `conflux_keys{provider="p1",state="exhausted"} 1`) {
		t.Errorf("keys gauge not pushed for exhausted state:\n%s", out)
	}
	// Proxy-health gauge is keyed by the credential-stripped URL and reports 0
	// (tripped), proving the app masks the URL exactly as /_status does.
	if !strings.Contains(out, `conflux_proxy_healthy{proxy="http://proxy1:8080"} 0`) {
		t.Errorf("proxy-healthy gauge not pushed / not masked:\n%s", out)
	}
	if strings.Contains(out, "user:pass@proxy1") {
		t.Errorf("proxy gauge leaked credentials:\n%s", out)
	}
}

// TestReloadSwapsRoutingAndPreservesState verifies that Reload re-reads the
// config file, swaps the routing table and pools, preserves the current
// in-memory key state (exhaustion carries over by hash), and keeps the
// metrics registry intact.
func TestReloadSwapsRoutingAndPreservesState(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	statePath := filepath.Join(dir, "state.yaml")

	// Initial config: one provider "p" with a catch-all model and two keys.
	initial := `
server:
  port: 24118
auth:
  client_keys: ["ck"]
providers:
  p:
    base_url: "http://up.test"
    keys:
      - key: "key-1"
      - key: "key-2"
    models:
      - "*"
    max_errors: 2
    cooldown: 1h
persistence:
  path: ` + statePath + `
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Exhaust key #1 in memory.
	pool := a.Pools["p"]
	pool.RecordError(1)
	pool.RecordError(1)
	if c := pool.Counts(); c.Exhausted != 1 {
		t.Fatalf("pre-reload counts = %+v, want 1 exhausted", c)
	}

	// The live snapshot routes any model to "p".
	live := a.Live.Load()
	if got, _ := live.Registry.Match("anything"); got != "p" {
		t.Fatalf("pre-reload route = %q, want p", got)
	}

	// Rewrite the config: add a second provider "q" for a specific model, and
	// keep the same two keys so their exhaustion carries over.
	updated := `
server:
  port: 24118
auth:
  client_keys: ["ck"]
providers:
  p:
    base_url: "http://up.test"
    keys:
      - key: "key-1"
      - key: "key-2"
    models:
      - "*"
    max_errors: 2
    cooldown: 1h
  q:
    base_url: "http://q.test"
    keys:
      - key: "qkey-1"
    models:
      - q-model
    max_errors: 2
    cooldown: 1h
persistence:
  path: ` + statePath + `
`
	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.Reload(cfgPath); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Routing now distinguishes q-model -> q, others -> p.
	live2 := a.Live.Load()
	if got, _ := live2.Registry.Match("q-model"); got != "q" {
		t.Fatalf("post-reload route(q-model) = %q, want q", got)
	}
	if got, _ := live2.Registry.Match("anything"); got != "p" {
		t.Fatalf("post-reload route(anything) = %q, want p", got)
	}
	// Pools now include q.
	if _, ok := live2.Pools["q"]; !ok {
		t.Error("post-reload live snapshot missing pool q")
	}

	// Key #1 exhaustion carried over (same key value -> same hash).
	pool2 := live2.Pools["p"]
	if c := pool2.Counts(); c.Exhausted != 1 {
		t.Fatalf("post-reload p counts = %+v, want 1 exhausted carried over", c)
	}

	// App-level mirror fields updated.
	if a.Registry != live2.Registry {
		t.Error("App.Registry should mirror the reloaded registry")
	}
	if _, ok := a.Pools["q"]; !ok {
		t.Error("App.Pools should mirror the reloaded pools")
	}
}
