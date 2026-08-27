package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// rawConfig is the raw YAML schema. yaml.v3 strict decoding
// (KnownFields) rejects unknown keys. Pointer fields distinguish absent
// (nil) from a zero value (non-nil), which is how the "first present wins"
// inheritance chain is implemented.
type rawConfig struct {
	Server   *rawServer      `yaml:"server"`
	Auth     *rawAuth        `yaml:"auth"`
	Logging  *rawLogging     `yaml:"logging"`
	Proxies  *rawGlobalProxy `yaml:"proxies"`
	Defaults *rawDefaults    `yaml:"defaults"`
	// Providers is captured as a raw node with document order preserved so
	// each entry can be decoded strictly while keeping the declaration order
	// for keyNumber.
	Providers   providersNode   `yaml:"providers"`
	Persistence *rawPersistence `yaml:"persistence"`
}

// providersNode captures the providers mapping node verbatim. It
// implements yaml.Unmarshaler so the top-level strict decoder does not try
// to enforce known fields against an opaque yaml.Node, which would reject the
// provider names themselves.
type providersNode struct {
	node *yaml.Node
}

func (p *providersNode) UnmarshalYAML(value *yaml.Node) error {
	p.node = value
	return nil
}

type rawServer struct {
	Port              *int    `yaml:"port"`
	ExposeDiagnostics *bool   `yaml:"expose_diagnostics"`
	AdminToken        *string `yaml:"admin_token"`
}

type rawAuth struct {
	ClientKeys []string `yaml:"client_keys"`
}

type rawLogging struct {
	Level   *string `yaml:"level"`
	MaxDirs *int    `yaml:"max_dirs"`
}

type rawGlobalProxy struct {
	URLs           []string `yaml:"urls"`
	RotateInterval *int     `yaml:"rotate_interval"`
	MaxErrors      *int     `yaml:"max_errors"`
	Cooldown       *string  `yaml:"cooldown"`
}

// rawPolicy contains the policy fields that appear in both defaults and
// providers. Embedding it into rawDefaults and rawProvider collapses the
// duplicated YAML schema so a new policy field is declared once.
type rawPolicy struct {
	KeySelection         *rawKeySelection  `yaml:"key_selection"`
	ActiveWindow         *int              `yaml:"active_window"`
	MaxErrors            *int              `yaml:"max_errors"`
	Cooldown             *string           `yaml:"cooldown"`
	RetireOnExhaustion   *bool             `yaml:"retire_on_exhaustion"`
	MaxStreamRetries     *int              `yaml:"max_stream_retries"`
	Upstream5xxThreshold *int              `yaml:"upstream_5xx_threshold"`
	Upstream5xxCooldown  *string           `yaml:"upstream_5xx_cooldown"`
	RateLimitRPM         *int              `yaml:"rate_limit_rpm"`
	Retry                *rawRetry          `yaml:"retry"`
	FallbackModels       map[string]string  `yaml:"fallback_models"`
	HeaderMasking        *rawHeaderMasking  `yaml:"header_masking"`
}

type rawHeaderMasking struct {
	Mode          *string           `yaml:"mode"`
	Profile       *string           `yaml:"profile"`
	Profiles      []string          `yaml:"profiles"`
	CustomHeaders map[string]string `yaml:"custom_headers"`
}

type rawDefaults struct {
	rawPolicy `yaml:",inline"`
}

type rawKeySelection struct {
	Mode           *string `yaml:"mode"`
	RequestsPerKey *int    `yaml:"requests_per_key"`
}

type rawRetry struct {
	MaxAttempts *int `yaml:"max_attempts"`
}

type rawProvider struct {
	Name      string        `yaml:"-"` // set from the map key
	BaseURL   string        `yaml:"base_url"`
	Models    []string      `yaml:"models"`
	Keys      []rawKey      `yaml:"keys"`
	Proxies   *rawProvProxy `yaml:"proxies"`
	rawPolicy `yaml:",inline"`
	// Per-provider client_keys is explicitly rejected.
	ClientKeys []string `yaml:"client_keys"`
}

type rawKey struct {
	Key     string `yaml:"key"`
	Proxy   string `yaml:"proxy"`
	Profile string `yaml:"profile"`
}

// rawProvProxy uses the canonical object form. A YAML array for `proxies`
// is rejected because it does not unmarshal into this struct: an array
// shorthand is a startup error.
type rawProvProxy struct {
	URLs           []string `yaml:"urls"`
	RotateInterval *int     `yaml:"rotate_interval"`
	MaxErrors      *int     `yaml:"max_errors"`
	Cooldown       *string  `yaml:"cooldown"`
}

type rawPersistence struct {
	Path string `yaml:"path"`
}

// Load reads and fully resolves the config at path. It fails fast on a
// missing file, invalid YAML, unknown keys, or any validation violation.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return parse(data)
}

// Parse decodes and resolves a config document from bytes. Parse is exported
// so tests can build a Config without touching the filesystem.
func Parse(data []byte) (*Config, error) { return parse(data) }

// parse decodes and resolves a config document. It is kept package-private so
// the seam stays at Load.
func parse(data []byte) (*Config, error) {
	var raw rawConfig
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // reject unknown keys
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnknownKey, err)
	}
	return resolve(&raw)
}

// ConfigPath returns the effective config path. The CONFIG_PATH
// environment variable overrides the default "config.yaml".
func ConfigPath() string {
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		return p
	}
	return "config.yaml"
}

// decodeProviders walks a providers mapping node in document order,
// decoding each value into a rawProvider and stamping the map key as the
// provider name. Document order is preserved so the 1-based declaration
// index stays stable for keyNumber. Each value is decoded with
// KnownFields(true) so unknown per-provider keys are rejected.
func decodeProviders(node *yaml.Node) ([]rawProvider, error) {
	// A mapping node's Content alternates key, value, key, value, and so on.
	out := make([]rawProvider, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		var p rawProvider
		// yaml.Node.Decode uses the std strictness of the node itself. To
		// reject unknown per-provider keys, the value node is re-encoded and
		// re-decoded with KnownFields. Direct node.Decode(&p) does not enforce
		// KnownFields, so the value node is marshaled to bytes and a strict
		// decoder runs over it.
		b, err := yaml.Marshal(valNode)
		if err != nil {
			return nil, fmt.Errorf("providers.%s: %w", keyNode.Value, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("providers.%s: %w", keyNode.Value, err)
		}
		p.Name = keyNode.Value
		out = append(out, p)
	}
	return out, nil
}
