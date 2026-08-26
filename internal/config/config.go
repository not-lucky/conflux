package config

import "time"

// Config is the fully resolved and validated configuration tree. Every
// field a runtime package needs is present here directly, so there is no
// defaults lookup at runtime. Provider policy fields are resolved with the
// inheritance chain providers.<name>.<field>, then defaults.<field>, then the
// built-in default.
type Config struct {
	Server   ServerConfig
	Auth     AuthConfig
	Logging  LoggingConfig
	Proxies  GlobalProxyConfig
	Defaults DefaultsConfig
	// Providers is an ordered slice; the order matches the declaration order in
	// the YAML file so the 1-based keyNumber stays stable.
	Providers []Provider
	// Persistence is optional; a nil path means no state file.
	Persistence *PersistenceConfig
}

type ServerConfig struct {
	Port              int
	ExposeDiagnostics bool
	AdminToken        string // empty when absent, which makes /admin/reload always return 401
}

type AuthConfig struct {
	// ClientKeys is the global gateway key set. Any entry grants access to
	// all providers and models through native passthrough.
	ClientKeys []string
}

type LoggingConfig struct {
	Level   string // "full", "errors_only", or "off"
	MaxDirs int
}

// GlobalProxyConfig is the global egress pool. Empty URLs mean no global
// proxy, so requests go direct.
type GlobalProxyConfig struct {
	URLs           []string
	RotateInterval int // 0 means no rotation (sticky pinning)
	MaxErrors      int
	Cooldown       time.Duration
}

// PolicyFields is the shared set of policy fields that appear in both
// DefaultsConfig and Provider. Embedding it collapses the duplicated
// field declarations so a single applyPolicyFields path can write to either
// resolved struct via a *PolicyFields pointer.
type PolicyFields struct {
	KeySelection          KeySelection
	ActiveWindow          int // 0 means "all keys"; EffectiveActiveWindow is the resolved value
	EffectiveActiveWindow int // active window with the 0=>all-keys rule and len(keys) clamp applied
	MaxErrors             int
	Cooldown              time.Duration
	RetireOnExhaustion    bool
	MaxStreamRetries      int
	Upstream5xxThreshold  int
	Upstream5xxCooldown   time.Duration
	RateLimitRPM          int
	RetryMaxAttempts      int
	FallbackModels        map[string]string
}

type DefaultsConfig struct {
	PolicyFields
}

type KeySelection struct {
	Mode           string // "round_robin" or "sticky"
	RequestsPerKey int
}

// PersistenceConfig is optional.
type PersistenceConfig struct {
	Path string
}

// Provider is a fully resolved provider entry. The effective values are
// baked in.
type Provider struct {
	Name    string
	BaseURL string
	Models  []ModelEntry // parsed patterns
	Keys    []Key
	Proxies *ProviderProxyConfig // nil means "use global"
	PolicyFields
}

// ModelEntry is a parsed model pattern. Kind distinguishes exact, prefix, and
// catch-all. Literal is the trimmed pattern string (without trailing "*").
type ModelEntry struct {
	Kind    ModelKind
	Literal string
}

type ModelKind int

const (
	ModelExact ModelKind = iota
	ModelPrefix
	ModelCatchAll
)

// Key is a provider key with an optional inline proxy override.
type Key struct {
	Value string
	Proxy string // empty when there is no inline override
}

// ProviderProxyConfig is a provider-scoped proxy pool that overrides the
// global pool. Only the canonical object form is accepted; an array
// shorthand is a startup error.
type ProviderProxyConfig struct {
	URLs           []string
	RotateInterval int
	MaxErrors      int
	Cooldown       time.Duration
}

// ProviderByName indexes the resolved providers by name.
func (c *Config) ProviderByName(name string) (*Provider, bool) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// ProxyURLs collects the full set of proxy URLs from the config, deduplicated
// preserving order: global pool URLs, each provider's pool URLs (when
// non-nil), and each key's inline proxy (when non-empty). This is the single
// canonical definition of the proxy-URL set, shared by persistence,
// metrics gauges, and /_status.
func (c *Config) ProxyURLs() []string {
	urlSet := map[string]bool{}
	var urls []string
	add := func(u string) {
		if u != "" && !urlSet[u] {
			urlSet[u] = true
			urls = append(urls, u)
		}
	}
	for _, u := range c.Proxies.URLs {
		add(u)
	}
	for _, p := range c.Providers {
		if p.Proxies != nil {
			for _, u := range p.Proxies.URLs {
				add(u)
			}
		}
		for _, k := range p.Keys {
			add(k.Proxy)
		}
	}
	return urls
}
