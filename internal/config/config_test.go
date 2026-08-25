package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const minimalYAML = `
server:
  port: 24118
  request_timeout: 60s
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
	if cfg.Server.RequestTimeout != 60*time.Second {
		t.Errorf("request_timeout = %v", cfg.Server.RequestTimeout)
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
	if oi.RequestTimeout != 60*time.Second {
		t.Errorf("request_timeout = %v", oi.RequestTimeout)
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
		{`server: {request_timeout: 60}`, ErrBadDuration}, // bare number
		{`server: {request_timeout: "0s"}`, ErrDurationNotPositive},
		{`server: {request_timeout: "10"}`, ErrBadDuration},   // no unit
		{`server: {request_timeout: "1.5s"}`, ErrBadDuration}, // float
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
		{"colon", `"a:b"`},
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
