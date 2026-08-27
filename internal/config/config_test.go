package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const minimalYAML = `
server:
  port: 24118
auth:
  client_keys:
    - sk-conflux-global-001
providers:
  openai:
    base_url: https://api.openai.com/v1
    models:
      - gpt-4o
      - gpt-4o-mini
      - gpt-4*
    keys:
      - key: sk-proj-a
      - key: sk-proj-b
        proxy: http://inline:8080
  anthropic:
    base_url: https://api.anthropic.com
    models:
      - claude-3-5-sonnet-20241022
      - claude-3*
    keys:
      - key: sk-ant-a
`

func TestParseMinimal(t *testing.T) {
	cfg, err := parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Server.Port != 24118 {
		t.Errorf("port = %d", cfg.Server.Port)
	}
	if len(cfg.Auth.ClientKeys) != 1 || cfg.Auth.ClientKeys[0] != "sk-conflux-global-001" {
		t.Errorf("client_keys = %v", cfg.Auth.ClientKeys)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %d", len(cfg.Providers))
	}
	oi, ok := cfg.ProviderByName("openai")
	if !ok {
		t.Fatal("openai not found")
	}
	if oi.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("base_url = %q (trailing slash?)", oi.BaseURL)
	}
	if len(oi.Models) != 3 {
		t.Fatalf("models = %d", len(oi.Models))
	}
	if oi.Models[0].Kind != ModelExact || oi.Models[0].Literal != "gpt-4o" {
		t.Errorf("models[0] = %+v", oi.Models[0])
	}
	if oi.Models[2].Kind != ModelPrefix || oi.Models[2].Literal != "gpt-4" {
		t.Errorf("models[2] = %+v", oi.Models[2])
	}
	if len(oi.Keys) != 2 {
		t.Fatalf("keys = %d", len(oi.Keys))
	}
	if oi.Keys[1].Proxy != "http://inline:8080" {
		t.Errorf("key[1].proxy = %q", oi.Keys[1].Proxy)
	}
	// Effective values inherited from defaults.
	if oi.MaxErrors != 5 {
		t.Errorf("max_errors = %d (want default 5)", oi.MaxErrors)
	}
	if oi.KeySelection.Mode != "round_robin" {
		t.Errorf("mode = %q", oi.KeySelection.Mode)
	}
}

func TestParseUnknownKey(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
unknown_toplevel: 1
`
	_, err := parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected unknown-key error")
	}
	if !errors.Is(err, ErrUnknownKey) {
		t.Errorf("err = %v, want ErrUnknownKey", err)
	}
}

func TestParseDurationErrors(t *testing.T) {
	cases := []struct {
		yaml string
		want error
	}{
		{`defaults: {cooldown: 60}`, ErrBadDuration}, // bare number
		{`defaults: {cooldown: "0s"}`, ErrDurationNotPositive},
		{`defaults: {cooldown: "10"}`, ErrBadDuration},   // no unit
		{`defaults: {cooldown: "1.5s"}`, ErrBadDuration}, // float
	}
	for i, c := range cases {
		yaml := c.yaml + "\nauth: {client_keys: [sk-1]}\nproviders:\n  p:\n    base_url: https://x.com\n    models: [m]\n    keys: [{key: k}]\n"
		_, err := parse([]byte(yaml))
		if err == nil {
			t.Errorf("case %d: expected error", i)
			continue
		}
		if !errors.Is(err, c.want) {
			t.Errorf("case %d: err = %v, want %v", i, err, c.want)
		}
	}
}

func TestParseAuthErrors(t *testing.T) {
	cases := map[string]string{
		"missing": `
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
`,
		"empty": `
auth:
  client_keys: []
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
`,
		"wildcard": `
auth:
  client_keys: ["*"]
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
`,
		"dup": `
auth:
  client_keys: [sk-1, sk-1]
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
`,
		"empty_key": `
auth:
  client_keys: [""]
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
`,
	}
	for name, yaml := range cases {
		_, err := parse([]byte(yaml))
		if err == nil || !errors.Is(err, ErrAuthError) {
			t.Errorf("case %q: err = %v, want ErrAuthError", name, err)
		}
	}
}

func TestParseAdminTokenOverlap(t *testing.T) {
	yaml := `
server: {admin_token: sk-shared}
auth: {client_keys: [sk-shared]}
providers: {p: {base_url: https://x.com, models: [m], keys: [{key: k}]}}
`
	_, err := parse([]byte(yaml))
	if err == nil || !errors.Is(err, ErrAuthError) {
		t.Errorf("err = %v, want ErrAuthError for admin_token overlap", err)
	}
}

func TestParsePerProviderClientKeysRejected(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
    client_keys: [sk-2]
`
	_, err := parse([]byte(yaml))
	if err == nil || !errors.Is(err, ErrAuthError) {
		t.Errorf("err = %v, want ErrAuthError for per-provider client_keys", err)
	}
}

func TestParseModelErrors(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"leading_star", `"*foo"`},
		{"middle_star", `"a*b"`},
		{"whitespace", `"a b"`},
		{"empty", `""`},
	}
	for _, c := range cases {
		yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [` + c.entry + `]
    keys: [{key: k}]
`
		_, err := parse([]byte(yaml))
		if err == nil || !errors.Is(err, ErrInvalidModel) {
			t.Errorf("case %q: err = %v, want ErrInvalidModel", c.name, err)
		}
	}
}

// TestParseModelAllowsColon verifies that a model id containing ':' parses as
// an exact match. OpenRouter-style ids such as
// "nvidia/nemotron-3.5-lightning:free" carry a ':' that must not be rejected
// or treated as a delimiter.
func TestParseModelAllowsColon(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: ["nvidia/nemotron-3.5-lightning:free"]
    keys: [{key: k}]
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := cfg.ProviderByName("p")
	if !ok {
		t.Fatal("provider p not found")
	}
	if len(p.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(p.Models))
	}
	if p.Models[0].Kind != ModelExact {
		t.Errorf("kind = %v, want ModelExact", p.Models[0].Kind)
	}
	if p.Models[0].Literal != "nvidia/nemotron-3.5-lightning:free" {
		t.Errorf("literal = %q", p.Models[0].Literal)
	}
}

func TestParseDuplicateModelsAcrossProviders(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  a:
    base_url: https://x.com
    models: [gpt-4o]
    keys: [{key: k}]
  b:
    base_url: https://y.com
    models: [gpt-4o]
    keys: [{key: k}]
`
	_, err := parse([]byte(yaml))
	if err == nil || !errors.Is(err, ErrDuplicateModel) {
		t.Errorf("err = %v, want ErrDuplicateModel", err)
	}
}

func TestParseDuplicatePrefixAcrossProviders(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  a:
    base_url: https://x.com
    models: [gpt-4*]
    keys: [{key: k}]
  b:
    base_url: https://y.com
    models: [gpt-4*]
    keys: [{key: k}]
`
	_, err := parse([]byte(yaml))
	if err == nil || !errors.Is(err, ErrDuplicateModel) {
		t.Errorf("err = %v, want ErrDuplicateModel", err)
	}
}

func TestParseExactPrefixOverlapAllowed(t *testing.T) {
	// An exact model in B plus a prefix in A is allowed: exact wins.
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  a:
    base_url: https://x.com
    models: [gpt-4*]
    keys: [{key: k}]
  b:
    base_url: https://y.com
    models: [gpt-4o]
    keys: [{key: k}]
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %d", len(cfg.Providers))
	}
}

func TestParseCatchAllMixedRejected(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: ["*", gpt-4]
    keys: [{key: k}]
`
	_, err := parse([]byte(yaml))
	if err == nil || !errors.Is(err, ErrInvalidModel) {
		t.Errorf("err = %v, want ErrInvalidModel", err)
	}
}

func TestParseTwoCatchAllRejected(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  a:
    base_url: https://x.com
    models: ["*"]
    keys: [{key: k}]
  b:
    base_url: https://y.com
    models: ["*"]
    keys: [{key: k}]
`
	_, err := parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for two catch-all providers")
	}
}

func TestParseDuplicateKeyWithinProvider(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: sk-same}, {key: sk-same}]
`
	_, err := parse([]byte(yaml))
	if err == nil || !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("err = %v, want ErrDuplicateKey", err)
	}
}

func TestParseActiveWindowTooLarge(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
    active_window: 5
`
	_, err := parse([]byte(yaml))
	if err == nil || !errors.Is(err, ErrActiveWindowError) {
		t.Errorf("err = %v, want ErrActiveWindowError", err)
	}
}

// TestParseEffectiveActiveWindow pins the single-source-of-truth derivation:
// config.resolve bakes EffectiveActiveWindow once (0=>all keys, clamped to
// len(keys)) so downstream consumers (keypool, server, app, dashboard) read one
// field instead of re-deriving the rule.
func TestParseEffectiveActiveWindow(t *testing.T) {
	cases := []struct {
		name  string
		aw    int // configured active_window; 0 = unset
		nKeys int
		want  int
	}{
		{"unset means all keys", 0, 3, 3},
		{"explicit equals keys", 2, 2, 2},
		{"explicit less than keys", 1, 4, 1},
		{"single key", 0, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keys := make([]string, 0, c.nKeys)
			for i := 0; i < c.nKeys; i++ {
				keys = append(keys, fmt.Sprintf("{key: k%d}", i))
			}
			awLine := ""
			if c.aw != 0 {
				awLine = fmt.Sprintf("active_window: %d", c.aw)
			}
			yaml := fmt.Sprintf(`
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [%s]
    %s
`, strings.Join(keys, ","), awLine)
			cfg, err := parse([]byte(yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			p := cfg.Providers[0]
			if p.EffectiveActiveWindow != c.want {
				t.Errorf("EffectiveActiveWindow = %d, want %d (ActiveWindow=%d, len(Keys)=%d)",
					p.EffectiveActiveWindow, c.want, p.ActiveWindow, len(p.Keys))
			}
		})
	}
}

func TestParseStickyWithRequestsPerKeyRejected(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
    key_selection: {mode: sticky, requests_per_key: 2}
`
	_, err := parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for sticky + explicit requests_per_key")
	}
	if !strings.Contains(err.Error(), "requests_per_key") {
		t.Errorf("err = %v", err)
	}
}

func TestParseProxyShape(t *testing.T) {
	// An array shorthand for provider proxies must error because it does not
	// unmarshal into the object struct.
	yaml := `
auth: {client_keys: [sk-1]}
proxies:
  urls: [http://p:8080]
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
    proxies: [http://a:8080]
`
	_, err := parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for array-form provider proxies")
	}
}

func TestParseFallbackModels(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
defaults:
  fallback_models:
    gpt-4: gpt-4o
providers:
  p:
    base_url: https://x.com
    models: [gpt-4, gpt-4o]
    keys: [{key: k}]
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Defaults.FallbackModels["gpt-4"] != "gpt-4o" {
		t.Errorf("fallback = %v", cfg.Defaults.FallbackModels)
	}
	p, _ := cfg.ProviderByName("p")
	if p.FallbackModels["gpt-4"] != "gpt-4o" {
		t.Errorf("provider fallback inherited = %v", p.FallbackModels)
	}
}

func TestParseRetryInheritance(t *testing.T) {
	yaml := `
auth: {client_keys: [sk-1]}
defaults:
  retry: {max_attempts: 4}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}, {key: k2}]
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, _ := cfg.ProviderByName("p")
	if p.RetryMaxAttempts != 4 {
		t.Errorf("retry.max_attempts = %d (want inherited 4)", p.RetryMaxAttempts)
	}
}

func TestParseHeaderMasking(t *testing.T) {
	t.Run("default passthrough", func(t *testing.T) {
		yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
`
		cfg, err := parse([]byte(yaml))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		p, _ := cfg.ProviderByName("p")
		if p.HeaderMasking.Mode != "passthrough" {
			t.Errorf("expected default mode passthrough, got %q", p.HeaderMasking.Mode)
		}
	})

	t.Run("defaults and provider override", func(t *testing.T) {
		yaml := `
auth: {client_keys: [sk-1]}
defaults:
  header_masking:
    mode: random
    profiles: [cursor, zed]
providers:
  p1:
    base_url: https://x.com
    models: [m1]
    keys: [{key: k1}]
  p2:
    base_url: https://y.com
    models: [m2]
    keys: [{key: k2}]
    header_masking:
      mode: profile
      profile: claude-code
      custom_headers:
        X-Custom: value
`
		cfg, err := parse([]byte(yaml))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		p1, _ := cfg.ProviderByName("p1")
		if p1.HeaderMasking.Mode != "random" || len(p1.HeaderMasking.Profiles) != 2 {
			t.Errorf("p1 unexpected header_masking: %+v", p1.HeaderMasking)
		}

		p2, _ := cfg.ProviderByName("p2")
		if p2.HeaderMasking.Mode != "profile" || p2.HeaderMasking.Profile != "claude-code" || p2.HeaderMasking.CustomHeaders["X-Custom"] != "value" {
			t.Errorf("p2 unexpected header_masking: %+v", p2.HeaderMasking)
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		yaml := `
auth: {client_keys: [sk-1]}
defaults:
  header_masking:
    mode: invalid_mode
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys: [{key: k}]
`
		_, err := parse([]byte(yaml))
		if err == nil {
			t.Fatal("expected error for invalid header_masking mode")
		}
	})

	t.Run("key bound profile", func(t *testing.T) {
		yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys:
      - key: k1
        profile: opencode
      - key: k2
        profile: cursor
`
		cfg, err := parse([]byte(yaml))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		p, _ := cfg.ProviderByName("p")
		if p.Keys[0].Profile != "opencode" {
			t.Errorf("k1 profile = %q, want opencode", p.Keys[0].Profile)
		}
		if p.Keys[1].Profile != "cursor" {
			t.Errorf("k2 profile = %q, want cursor", p.Keys[1].Profile)
		}
	})

	t.Run("key bound invalid profile", func(t *testing.T) {
		yaml := `
auth: {client_keys: [sk-1]}
providers:
  p:
    base_url: https://x.com
    models: [m]
    keys:
      - key: k1
        profile: invalid_profile_name
`
		_, err := parse([]byte(yaml))
		if err == nil {
			t.Fatal("expected error for invalid key profile")
		}
	})
}

