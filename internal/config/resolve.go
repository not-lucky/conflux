package config

import (
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// resolve decodes the raw tree, runs the full validation matrix, and
// produces a fully resolved *Config with the effective values baked in.
func resolve(raw *rawConfig) (*Config, error) {
	cfg := &Config{}

	// --- server ---
	server, err := resolveServer(raw.Server)
	if err != nil {
		return nil, err
	}
	cfg.Server = server

	// --- auth (required) ---
	if raw.Auth == nil {
		return nil, wrapf(ErrAuthError, "auth", "missing auth section")
	}
	if err := resolveAuth(raw.Auth, cfg.Server.AdminToken); err != nil {
		return nil, err
	}
	cfg.Auth = AuthConfig{ClientKeys: raw.Auth.ClientKeys}

	// --- logging ---
	logging, err := resolveLogging(raw.Logging)
	if err != nil {
		return nil, err
	}
	cfg.Logging = logging

	// --- global proxies ---
	gp, err := resolveGlobalProxy(raw.Proxies)
	if err != nil {
		return nil, err
	}
	cfg.Proxies = gp

	// --- defaults ---
	def, err := resolveDefaults(raw.Defaults, cfg.Server)
	if err != nil {
		return nil, err
	}
	cfg.Defaults = def

	// --- providers (required, at least one) ---
	if raw.Providers.node == nil || raw.Providers.node.Kind != yaml.MappingNode || len(raw.Providers.node.Content) == 0 {
		return nil, wrapf(ErrProviderError, "providers", "at least one provider required")
	}
	ordered, err := decodeProviders(raw.Providers.node)
	if err != nil {
		return nil, err
	}
	provNames := map[string]bool{}
	catchAllCount := 0
	exactGlobal := map[string]string{}  // exact id -> provider name
	prefixGlobal := map[string]string{} // prefix -> provider name
	cfg.Providers = make([]Provider, 0, len(ordered))
	for i := range ordered {
		p, err := resolveProvider(&ordered[i], def, cfg.Server, cfg.Proxies)
		if err != nil {
			return nil, err
		}
		// name uniqueness
		if provNames[p.Name] {
			return nil, wrapf(ErrProviderError, "providers", "duplicate provider name %q", p.Name)
		}
		provNames[p.Name] = true
		// cross-provider model overlap checks
		for _, m := range p.Models {
			switch m.Kind {
			case ModelCatchAll:
				catchAllCount++
				if catchAllCount > 1 {
					return nil, wrapf(ErrProviderError, "providers", "at most one catch-all provider (models: [\"*\"])")
				}
				if len(p.Models) != 1 {
					return nil, wrapf(ErrProviderError, "providers."+p.Name, "catch-all provider models must be exactly [\"*\"]")
				}
			case ModelExact:
				if other, ok := exactGlobal[m.Literal]; ok && other != p.Name {
					return nil, wrapf(ErrDuplicateModel, "providers", "exact model %q declared in %q and %q", m.Literal, other, p.Name)
				}
				exactGlobal[m.Literal] = p.Name
			case ModelPrefix:
				if other, ok := prefixGlobal[m.Literal]; ok && other != p.Name {
					return nil, wrapf(ErrDuplicateModel, "providers", "prefix %q* declared in %q and %q", m.Literal, other, p.Name)
				}
				prefixGlobal[m.Literal] = p.Name
			}
		}
		cfg.Providers = append(cfg.Providers, p)
	}

	// --- persistence (optional) ---
	if raw.Persistence != nil {
		cfg.Persistence = &PersistenceConfig{Path: raw.Persistence.Path}
	}

	return cfg, nil
}

func resolveServer(s *rawServer) (ServerConfig, error) {
	out := ServerConfig{
		Port:                    24118,
		RequestTimeout:          60 * time.Second,
		StreamIdleTimeout:       15 * time.Second,
		StreamKeepaliveInterval: 15 * time.Second,
		RequestDeadline:         180 * time.Second,
		ExposeDiagnostics:       true,
	}
	if s == nil {
		return out, nil
	}
	if s.Port != nil {
		p := *s.Port
		if p < 1 || p > 65535 {
			return ServerConfig{}, wrapf(ErrInvalidPort, "server.port", "must be 1-65535, got %d", p)
		}
		out.Port = p
	}
	if err := dur(&out.RequestTimeout, "server.request_timeout", s.RequestTimeout); err != nil {
		return ServerConfig{}, err
	}
	if err := dur(&out.StreamIdleTimeout, "server.stream_idle_timeout", s.StreamIdleTimeout); err != nil {
		return ServerConfig{}, err
	}
	if err := dur(&out.StreamKeepaliveInterval, "server.stream_keepalive_interval", s.StreamKeepaliveInterval); err != nil {
		return ServerConfig{}, err
	}
	if err := dur(&out.RequestDeadline, "server.request_deadline", s.RequestDeadline); err != nil {
		return ServerConfig{}, err
	}
	boolval(&out.ExposeDiagnostics, s.ExposeDiagnostics)
	if s.AdminToken != nil {
		out.AdminToken = strings.TrimSpace(*s.AdminToken)
	}
	return out, nil
}

func resolveAuth(a *rawAuth, adminToken string) error {
	if len(a.ClientKeys) == 0 {
		return wrapf(ErrAuthError, "auth.client_keys", "must have >=1 entry")
	}
	seen := map[string]bool{}
	for _, k := range a.ClientKeys {
		k = strings.TrimSpace(k)
		if k == "" {
			return wrapf(ErrAuthError, "auth.client_keys", "contains empty entry")
		}
		if k == "*" {
			return wrapf(ErrAuthError, "auth.client_keys", "\"*\" is not allowed (wildcard only for models)")
		}
		if seen[k] {
			return wrapf(ErrAuthError, "auth.client_keys", "duplicate client key %q", k)
		}
		seen[k] = true
	}
	// admin_token must not equal any client key
	if adminToken != "" {
		for _, k := range a.ClientKeys {
			if strings.TrimSpace(k) == adminToken {
				return wrapf(ErrAuthError, "server.admin_token", "must not equal any auth.client_keys entry")
			}
		}
	}
	return nil
}

func resolveLogging(l *rawLogging) (LoggingConfig, error) {
	out := LoggingConfig{Level: "full", MaxDirs: 1000}
	if l == nil {
		return out, nil
	}
	if l.Level != nil {
		switch *l.Level {
		case "full", "errors_only", "off":
			out.Level = *l.Level
		default:
			return LoggingConfig{}, wrapf(ErrUnknownKey, "logging.level", "must be full, errors_only, or off, got %q", *l.Level)
		}
	}
	if err := intval(&out.MaxDirs, "logging.max_dirs", l.MaxDirs, 0, ErrProviderError); err != nil {
		return LoggingConfig{}, err
	}
	return out, nil
}

func resolveGlobalProxy(g *rawGlobalProxy) (GlobalProxyConfig, error) {
	out := GlobalProxyConfig{MaxErrors: 3, Cooldown: 30 * time.Second}
	if g == nil {
		return out, nil
	}
	for _, u := range g.URLs {
		if err := validateProxyURL(u); err != nil {
			return GlobalProxyConfig{}, wrapf(err, "proxies.urls", "%v", err)
		}
		out.URLs = append(out.URLs, strings.TrimSpace(u))
	}
	if g.RotateInterval != nil {
		if err := intval(&out.RotateInterval, "proxies.rotate_interval", g.RotateInterval, 1, ErrProxyShape); err != nil {
			return GlobalProxyConfig{}, err
		}
	}
	if err := intval(&out.MaxErrors, "proxies.max_errors", g.MaxErrors, 1, ErrProxyShape); err != nil {
		return GlobalProxyConfig{}, err
	}
	if err := dur(&out.Cooldown, "proxies.cooldown", g.Cooldown); err != nil {
		return GlobalProxyConfig{}, err
	}
	return out, nil
}

func resolveDefaults(d *rawDefaults, server ServerConfig) (DefaultsConfig, error) {
	out := DefaultsConfig{PolicyFields: PolicyFields{
		KeySelection:            KeySelection{Mode: "round_robin", RequestsPerKey: 1},
		MaxErrors:               5,
		Cooldown:                5 * time.Hour,
		MaxStreamRetries:        3,
		Upstream5xxThreshold:    5,
		Upstream5xxCooldown:     30 * time.Second,
		RequestTimeout:          server.RequestTimeout,
		StreamIdleTimeout:       server.StreamIdleTimeout,
		StreamKeepaliveInterval: server.StreamKeepaliveInterval,
		RequestDeadline:         server.RequestDeadline,
	}}
	if d == nil {
		return out, nil
	}
	if err := applyPolicyFields(&out.PolicyFields, "defaults", "defaults", &d.rawPolicy); err != nil {
		return DefaultsConfig{}, err
	}
	return out, nil
}

// applyPolicyFields resolves the shared policy fields from raw into the
// PolicyFields value p points to. field is the field prefix used in error
// messages for the scalar fields (e.g. "defaults" or "providers.<name>").
// ksField is the prefix used for key_selection sub-fields, which differs
// between defaults ("defaults") and providers ("providers.<name>.key_selection").
// The first present value wins via the nil-pointer pattern in the raw structs,
// so the existing seed in *p is kept when a raw value is absent.
func applyPolicyFields(p *PolicyFields, field, ksField string, raw *rawPolicy) error {
	if raw.KeySelection != nil {
		ks, err := resolveKeySelection(raw.KeySelection, ksField)
		if err != nil {
			return err
		}
		p.KeySelection = ks
	}
	if err := intval(&p.ActiveWindow, field+".active_window", raw.ActiveWindow, 1, ErrActiveWindowError); err != nil {
		return err
	}
	if err := intval(&p.MaxErrors, field+".max_errors", raw.MaxErrors, 1, ErrProviderError); err != nil {
		return err
	}
	if err := dur(&p.Cooldown, field+".cooldown", raw.Cooldown); err != nil {
		return err
	}
	boolval(&p.RetireOnExhaustion, raw.RetireOnExhaustion)
	if err := intval(&p.MaxStreamRetries, field+".max_stream_retries", raw.MaxStreamRetries, 0, ErrProviderError); err != nil {
		return err
	}
	if err := intval(&p.Upstream5xxThreshold, field+".upstream_5xx_threshold", raw.Upstream5xxThreshold, 1, ErrProviderError); err != nil {
		return err
	}
	if err := dur(&p.Upstream5xxCooldown, field+".upstream_5xx_cooldown", raw.Upstream5xxCooldown); err != nil {
		return err
	}
	if err := dur(&p.RequestTimeout, field+".request_timeout", raw.RequestTimeout); err != nil {
		return err
	}
	if err := dur(&p.StreamIdleTimeout, field+".stream_idle_timeout", raw.StreamIdleTimeout); err != nil {
		return err
	}
	if err := dur(&p.StreamKeepaliveInterval, field+".stream_keepalive_interval", raw.StreamKeepaliveInterval); err != nil {
		return err
	}
	if err := dur(&p.RequestDeadline, field+".request_deadline", raw.RequestDeadline); err != nil {
		return err
	}
	if err := intval(&p.RateLimitRPM, field+".rate_limit_rpm", raw.RateLimitRPM, 1, ErrProviderError); err != nil {
		return err
	}
	if raw.Retry != nil && raw.Retry.MaxAttempts != nil {
		if err := intval(&p.RetryMaxAttempts, field+".retry.max_attempts", raw.Retry.MaxAttempts, 1, ErrRetryError); err != nil {
			return err
		}
	}
	if raw.FallbackModels != nil {
		if err := validateFallback(raw.FallbackModels, field+".fallback_models"); err != nil {
			return err
		}
		p.FallbackModels = raw.FallbackModels
	}
	return nil
}

func resolveKeySelection(ks *rawKeySelection, field string) (KeySelection, error) {
	out := KeySelection{Mode: "round_robin", RequestsPerKey: 1}
	if ks.Mode != nil {
		switch *ks.Mode {
		case "round_robin", "sticky":
			out.Mode = *ks.Mode
		default:
			return KeySelection{}, wrapf(ErrProviderError, field+".mode", "must be round_robin or sticky, got %q", *ks.Mode)
		}
	}
	if ks.RequestsPerKey != nil {
		if *ks.RequestsPerKey < 1 {
			return KeySelection{}, wrapf(ErrProviderError, field+".requests_per_key", "must be >=1, got %d", *ks.RequestsPerKey)
		}
		out.RequestsPerKey = *ks.RequestsPerKey
	}
	return out, nil
}

func validateFallback(m map[string]string, field string) error {
	for k, v := range m {
		if strings.TrimSpace(k) == "" {
			return wrapf(ErrFallbackError, field, "empty key")
		}
		if strings.TrimSpace(v) == "" {
			return wrapf(ErrFallbackError, field, "empty value for %q", k)
		}
	}
	return nil
}

func resolveProvider(p *rawProvider, def DefaultsConfig, server ServerConfig, global GlobalProxyConfig) (Provider, error) {
	if p.Name == "" {
		return Provider{}, wrapf(ErrProviderError, "providers", "provider with empty name")
	}
	if strings.ContainsAny(p.Name, " \t:") {
		return Provider{}, wrapf(ErrProviderError, "providers."+p.Name, "name must not contain whitespace or ':'")
	}
	// Per-provider client_keys is rejected.
	if len(p.ClientKeys) > 0 {
		return Provider{}, wrapf(ErrAuthError, "providers."+p.Name, "per-provider client_keys is not supported (use models: [\"*\"] for catch-all)")
	}
	if err := validateBaseURL(p.BaseURL); err != nil {
		return Provider{}, wrapf(err, "providers."+p.Name, "%v", err)
	}
	// strip trailing slash after validation
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")

	// models
	if len(p.Models) == 0 {
		return Provider{}, wrapf(ErrProviderError, "providers."+p.Name, "models: must have >=1 entry")
	}
	entries := make([]ModelEntry, 0, len(p.Models))
	seenInProv := map[string]bool{}
	catchAllInProv := false
	for _, m := range p.Models {
		me, err := parseModelEntry(m)
		if err != nil {
			return Provider{}, wrapf(err, "providers."+p.Name, "%v", err)
		}
		key := me.Literal
		if me.Kind == ModelCatchAll {
			if catchAllInProv {
				return Provider{}, wrapf(ErrInvalidModel, "providers."+p.Name, "duplicate catch-all")
			}
			catchAllInProv = true
			// Mixed ["*", "gpt-4"] is an error: catch-all must be sole entry.
			if len(p.Models) != 1 {
				return Provider{}, wrapf(ErrInvalidModel, "providers."+p.Name, "catch-all \"*\" must be the sole models entry")
			}
		} else {
			if seenInProv[key] {
				return Provider{}, wrapf(ErrDuplicateModel, "providers."+p.Name, "duplicate model %q", m)
			}
			seenInProv[key] = true
		}
		entries = append(entries, me)
	}

	// keys
	if len(p.Keys) == 0 {
		return Provider{}, wrapf(ErrProviderError, "providers."+p.Name, "keys: must have >=1 entry")
	}
	keys := make([]Key, 0, len(p.Keys))
	keySeen := map[string]bool{}
	for _, rk := range p.Keys {
		if strings.TrimSpace(rk.Key) == "" {
			return Provider{}, wrapf(ErrProviderError, "providers."+p.Name, "keys[].key must be non-empty")
		}
		if keySeen[rk.Key] {
			return Provider{}, wrapf(ErrDuplicateKey, "providers."+p.Name, "duplicate key %q (shared SHA256 hash)", rk.Key)
		}
		keySeen[rk.Key] = true
		k := Key{Value: rk.Key}
		if strings.TrimSpace(rk.Proxy) != "" {
			if err := validateProxyURL(rk.Proxy); err != nil {
				return Provider{}, wrapf(err, "providers."+p.Name+".keys[].proxy", "%v", err)
			}
			k.Proxy = strings.TrimSpace(rk.Proxy)
		}
		keys = append(keys, k)
	}

	// proxies (provider-scoped)
	var ppc *ProviderProxyConfig
	if p.Proxies != nil {
		pp, err := resolveProviderProxy(p.Proxies, p.Name, global)
		if err != nil {
			return Provider{}, wrapf(err, "providers."+p.Name+".proxies", "%v", err)
		}
		ppc = &pp
	}

	out := Provider{
		Name: p.Name, BaseURL: base, Models: entries, Keys: keys, Proxies: ppc,
		PolicyFields: PolicyFields{
			KeySelection:            def.KeySelection,
			ActiveWindow:            def.ActiveWindow,
			MaxErrors:               def.MaxErrors,
			Cooldown:                def.Cooldown,
			RetireOnExhaustion:      def.RetireOnExhaustion,
			MaxStreamRetries:        def.MaxStreamRetries,
			Upstream5xxThreshold:    def.Upstream5xxThreshold,
			Upstream5xxCooldown:     def.Upstream5xxCooldown,
			RequestTimeout:          def.RequestTimeout,
			StreamIdleTimeout:       def.StreamIdleTimeout,
			StreamKeepaliveInterval: def.StreamKeepaliveInterval,
			RequestDeadline:         def.RequestDeadline,
			RateLimitRPM:            def.RateLimitRPM,
			RetryMaxAttempts:        def.RetryMaxAttempts,
			FallbackModels:          def.FallbackModels,
		},
	}

	// Shared policy fields: the first present value wins over the seeded
	// defaults. applyPolicyFields runs the shared dur/intval/boolval/key-
	// selection/fallback sequence once for both defaults and providers.
	if err := applyPolicyFields(&out.PolicyFields, "providers."+p.Name, "providers."+p.Name+".key_selection", &p.rawPolicy); err != nil {
		return Provider{}, err
	}

	// Per-provider key_selection post-processing: sticky with an explicit
	// requests_per_key is an error. When the effective mode is sticky, any
	// inherited RequestsPerKey is silently ignored (kept at 1 for round-
	// robin bookkeeping; sticky does not use it).
	if p.rawPolicy.KeySelection != nil && out.KeySelection.Mode == "sticky" && p.rawPolicy.KeySelection.RequestsPerKey != nil {
		return Provider{}, wrapf(ErrProviderError, "providers."+p.Name+".key_selection", "requests_per_key is not valid with mode: sticky")
	}
	if out.KeySelection.Mode == "sticky" {
		out.KeySelection.RequestsPerKey = 1
	}

	// Per-provider active_window: must not exceed the number of keys.
	if p.rawPolicy.ActiveWindow != nil && out.ActiveWindow > len(keys) {
		return Provider{}, wrapf(ErrActiveWindowError, "providers."+p.Name+".active_window", "must be <= len(keys) (%d), got %d", len(keys), out.ActiveWindow)
	}
	return out, nil
}

func resolveProviderProxy(pp *rawProvProxy, name string, global GlobalProxyConfig) (ProviderProxyConfig, error) {
	out := ProviderProxyConfig{
		MaxErrors: global.MaxErrors,
		Cooldown:  global.Cooldown,
	}
	for _, u := range pp.URLs {
		if err := validateProxyURL(u); err != nil {
			return ProviderProxyConfig{}, err
		}
		out.URLs = append(out.URLs, strings.TrimSpace(u))
	}
	if pp.RotateInterval != nil {
		if err := intval(&out.RotateInterval, "providers."+name+".proxies.rotate_interval", pp.RotateInterval, 1, ErrProxyShape); err != nil {
			return ProviderProxyConfig{}, err
		}
	} else {
		out.RotateInterval = global.RotateInterval
	}
	if err := intval(&out.MaxErrors, "providers."+name+".proxies.max_errors", pp.MaxErrors, 1, ErrProxyShape); err != nil {
		return ProviderProxyConfig{}, err
	}
	if err := dur(&out.Cooldown, "providers."+name+".proxies.cooldown", pp.Cooldown); err != nil {
		return ProviderProxyConfig{}, err
	}
	return out, nil
}
