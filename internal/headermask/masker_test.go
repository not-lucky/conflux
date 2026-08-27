package headermask

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestPassthroughMode(t *testing.T) {
	h := http.Header{
		"User-Agent":       []string{"Bun/1.4.0"},
		"X-Stainless-Lang": []string{"custom"},
		"Content-Type":     []string{"application/json"},
	}
	cfg := Config{
		Mode: "passthrough",
	}
	Apply(h, cfg)

	if h.Get("User-Agent") != "Bun/1.4.0" {
		t.Errorf("expected User-Agent preserved in passthrough, got %q", h.Get("User-Agent"))
	}
	if h.Get("X-Stainless-Lang") != "custom" {
		t.Errorf("expected X-Stainless-Lang preserved, got %q", h.Get("X-Stainless-Lang"))
	}
}

func TestStripIdentifyingHeaders(t *testing.T) {
	h := http.Header{
		"User-Agent":                  []string{"Bun/1.4.0"},
		"X-Stainless-Lang":            []string{"python"},
		"X-Stainless-Package-Version": []string{"1.2.3"},
		"X-Cursor-Client-Version":     []string{"0.40.0"},
		"X-Client-Version":            []string{"0.40.0"},
		"X-Aider-Test":                []string{"1"},
		"Editor-Version":              []string{"vscode/1.90.0"},
		"Editor-Plugin-Version":       []string{"copilot/1.0"},
		"X-Amzn-Trace-Id":             []string{"Root=1-12345678"},
		"Authorization":               []string{"Bearer secret"},
		"Content-Type":                []string{"application/json"},
	}

	StripIdentifyingHeaders(h)

	for _, stripped := range []string{
		"User-Agent",
		"X-Stainless-Lang",
		"X-Stainless-Package-Version",
		"X-Cursor-Client-Version",
		"X-Client-Version",
		"X-Aider-Test",
		"Editor-Version",
		"Editor-Plugin-Version",
		"X-Amzn-Trace-Id",
	} {
		if val := h.Get(stripped); val != "" {
			t.Errorf("header %q was not stripped, got %q", stripped, val)
		}
	}

	if h.Get("Authorization") != "Bearer secret" {
		t.Errorf("Authorization header unexpectedly modified: %q", h.Get("Authorization"))
	}
	if h.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type header unexpectedly modified: %q", h.Get("Content-Type"))
	}
}

func TestProfileMode(t *testing.T) {
	for _, name := range BuiltinProfileNames {
		h := http.Header{
			"User-Agent":       []string{"curl/7.68.0"},
			"X-Stainless-Lang": []string{"custom"},
		}
		cfg := Config{
			Mode:    "profile",
			Profile: name,
		}
		Apply(h, cfg)

		ua := h.Get("User-Agent")
		if ua == "" || ua == "curl/7.68.0" {
			t.Errorf("profile %q did not set valid User-Agent: %q", name, ua)
		}

		if name == "claude-code" {
			if h.Get("anthropic-version") != "2023-06-01" {
				t.Errorf("claude-code profile missing anthropic-version header")
			}
		}
		if name == "openai-python" {
			if h.Get("x-stainless-lang") != "python" {
				t.Errorf("openai-python profile missing x-stainless-lang")
			}
			if !strings.HasPrefix(h.Get("User-Agent"), "OpenAI/Python") {
				t.Errorf("openai-python profile unexpected User-Agent: %q", h.Get("User-Agent"))
			}
		}
		if name == "copilot" {
			if h.Get("editor-version") == "" || h.Get("editor-plugin-version") == "" {
				t.Errorf("copilot profile missing editor headers")
			}
		}
		if name == "opencode" {
			if !strings.HasPrefix(h.Get("User-Agent"), "opencode/") {
				t.Errorf("opencode profile unexpected User-Agent: %q", h.Get("User-Agent"))
			}
			if h.Get("x-opencode-version") == "" {
				t.Errorf("opencode profile missing x-opencode-version")
			}
			if h.Get("x-stainless-runtime") != "bun" {
				t.Errorf("opencode profile missing x-stainless-runtime bun")
			}
		}
		if name == "cline" {
			if h.Get("x-client-type") != "cline-cli" {
				t.Errorf("cline profile missing x-client-type cline-cli, got %q", h.Get("x-client-type"))
			}
			if !strings.HasPrefix(h.Get("User-Agent"), "Cline/") {
				t.Errorf("cline profile unexpected User-Agent: %q", h.Get("User-Agent"))
			}
			clHeaders := map[string]string{
				"HTTP-Referer":       "https://cline.bot",
				"X-Title":            "Cline",
				"X-Client-Version":   "3.0.60",
				"X-Platform":         "cli",
				"X-Platform-Version": "3.0.60",
				"X-Core-Version":     "0.0.81",
				"X-Is-Multiroot":     "false",
			}
			for k, want := range clHeaders {
				if got := h.Get(k); got != want {
					t.Errorf("cline profile header %s = %q, want %q", k, got, want)
				}
			}
			if h.Get("X-Task-ID") == "" || h.Get("X-Task-ID") == TaskIDPlaceholder {
				t.Errorf("cline profile did not resolve X-Task-ID: %q", h.Get("X-Task-ID"))
			}
		}
	}
}

func TestClineTaskIDDynamic(t *testing.T) {
	cfg := Config{
		Mode:    "profile",
		Profile: "cline",
	}
	re := regexp.MustCompile(`^\d+_[A-Za-z0-9_-]{5}$`)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		h := http.Header{}
		Apply(h, cfg)
		id := h.Get("X-Task-ID")
		if !re.MatchString(id) {
			t.Fatalf("X-Task-ID %q does not match Cline session id format", id)
		}
		if seen[id] {
			t.Fatalf("X-Task-ID %q repeated across requests", id)
		}
		seen[id] = true
	}
}

func TestRandomMode(t *testing.T) {
	seenAgents := map[string]int{}
	cfg := Config{
		Mode: "random",
	}

	for i := 0; i < 50; i++ {
		h := http.Header{
			"User-Agent": []string{"Bun/1.4.0"},
		}
		Apply(h, cfg)
		ua := h.Get("User-Agent")
		if ua == "" || ua == "Bun/1.4.0" {
			t.Fatalf("expected randomized User-Agent, got %q", ua)
		}
		seenAgents[ua]++
	}

	// Over 50 iterations, we should have seen multiple distinct User-Agents.
	if len(seenAgents) < 3 {
		t.Errorf("expected diverse User-Agents across 50 runs, got %d distinct: %v", len(seenAgents), seenAgents)
	}
}

func TestRandomModeWithRestrictedPool(t *testing.T) {
	cfg := Config{
		Mode:     "random",
		Profiles: []string{"cursor", "zed"},
	}

	for i := 0; i < 20; i++ {
		h := http.Header{
			"User-Agent": []string{"test-runner"},
		}
		Apply(h, cfg)
		ua := h.Get("User-Agent")
		if !strings.HasPrefix(ua, "Cursor/") && !strings.HasPrefix(ua, "Zed/") {
			t.Errorf("expected User-Agent from restricted pool (cursor or zed), got %q", ua)
		}
	}
}

func TestCustomHeadersOverride(t *testing.T) {
	h := http.Header{
		"User-Agent": []string{"Bun/1.4.0"},
	}
	cfg := Config{
		Mode:    "profile",
		Profile: "cursor",
		CustomHeaders: map[string]string{
			"X-Custom-Env": "staging",
			"User-Agent":   "CustomAgent/1.0",
		},
	}
	Apply(h, cfg)

	if h.Get("X-Custom-Env") != "staging" {
		t.Errorf("custom header X-Custom-Env not set")
	}
	if h.Get("User-Agent") != "CustomAgent/1.0" {
		t.Errorf("expected custom User-Agent override, got %q", h.Get("User-Agent"))
	}
}
