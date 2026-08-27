package headermask

import (
	"crypto/rand"
	"math/big"
)

// HeaderSet is a collection of key-value pairs representing authentic client headers.
type HeaderSet map[string]string

// TaskIDPlaceholder marks a profile header value that Apply replaces with a
// fresh per-request task identifier. Cline's gateway expects an X-Task-ID on
// every request; reusing one static id would correlate all proxied traffic
// into a single task.
const TaskIDPlaceholder = "{task-id}"

// Profile defines an authentic AI coding agent or SDK identity profile (Linux x86_64).
type Profile struct {
	Name        string    // e.g. "cursor", "claude-code", "cline"
	Description string    // human-readable description
	UserAgent   string    // authentic Linux x64 User-Agent
	ExtraHeader HeaderSet // additional headers specific to this client
}

// BuiltinProfiles contains the authentic header profiles for popular AI coding agents and SDKs on Linux x64.
var BuiltinProfiles = map[string]Profile{
	"cursor": {
		Name:        "cursor",
		Description: "Cursor AI Code Editor",
		UserAgent:   "Cursor/0.45.8 (linux; x64)",
		ExtraHeader: HeaderSet{
			"x-cursor-client-version": "0.45.8",
			"x-client-version":        "0.45.8",
		},
	},
	"claude-code": {
		Name:        "claude-code",
		Description: "Anthropic Claude Code CLI",
		UserAgent:   "claude-code/0.2.32 (Linux x86_64)",
		ExtraHeader: HeaderSet{
			"anthropic-version": "2023-06-01",
		},
	},
	"cline": {
		Name:        "cline",
		Description: "Cline CLI",
		// Authentic Cline CLI identity (cline/cline sdk/packages/llms
		// request-headers.ts). The Cline gateway at api.cline.bot requires
		// these client headers to serve free models. Versions track
		// @cline/cli 3.0.60 and @cline/core 0.0.81.
		UserAgent: "Cline/3.0.60",
		ExtraHeader: HeaderSet{
			"http-referer":       "https://cline.bot",
			"x-title":            "Cline",
			"x-client-type":      "cline-cli",
			"x-client-version":   "3.0.60",
			"x-platform":         "cli",
			"x-platform-version": "3.0.60",
			"x-core-version":     "0.0.81",
			"x-is-multiroot":     "false",
			"x-task-id":          TaskIDPlaceholder,
		},
	},
	"roo-code": {
		Name:        "roo-code",
		Description: "Roo Code VS Code Extension",
		UserAgent:   "Roo-Code/3.7.0 (vscode/1.96.0; linux/x64)",
	},
	"windsurf": {
		Name:        "windsurf",
		Description: "Windsurf / Codeium IDE",
		UserAgent:   "Windsurf/1.1.8 (linux; x64)",
	},
	"aider": {
		Name:        "aider",
		Description: "Aider AI Pair Programming CLI",
		UserAgent:   "aider/0.72.0 Python/3.11.9 (Linux-6.5.0-x86_64)",
	},
	"continue": {
		Name:        "continue",
		Description: "Continue.dev Extension",
		UserAgent:   "Continue/0.8.88 (vscode/1.96.2; linux/x64)",
	},
	"zed": {
		Name:        "zed",
		Description: "Zed Code Editor",
		UserAgent:   "Zed/0.169.2 (linux; x86_64)",
	},
	"copilot": {
		Name:        "copilot",
		Description: "GitHub Copilot (VS Code)",
		UserAgent:   "GithubCopilot/1.250.0",
		ExtraHeader: HeaderSet{
			"editor-version":        "vscode/1.96.2",
			"editor-plugin-version": "copilot/1.250.0",
		},
	},
	"openai-python": {
		Name:        "openai-python",
		Description: "Official OpenAI Python SDK",
		UserAgent:   "OpenAI/Python 1.60.0",
		ExtraHeader: HeaderSet{
			"x-stainless-lang":            "python",
			"x-stainless-package-version": "1.60.0",
			"x-stainless-os":              "Linux",
			"x-stainless-arch":            "x64",
			"x-stainless-runtime":         "CPython",
			"x-stainless-runtime-version": "3.11.9",
			"x-stainless-async":           "true",
		},
	},
	"openai-node": {
		Name:        "openai-node",
		Description: "Official OpenAI Node.js SDK",
		UserAgent:   "OpenAI/NodeJS 4.80.0",
		ExtraHeader: HeaderSet{
			"x-stainless-lang":            "js",
			"x-stainless-package-version": "4.80.0",
			"x-stainless-os":              "Linux",
			"x-stainless-arch":            "x64",
			"x-stainless-runtime":         "node",
			"x-stainless-runtime-version": "v20.18.0",
		},
	},
	"anthropic-python": {
		Name:        "anthropic-python",
		Description: "Official Anthropic Python SDK",
		UserAgent:   "anthropic-python/0.46.0",
		ExtraHeader: HeaderSet{
			"x-stainless-lang":            "python",
			"x-stainless-package-version": "0.46.0",
			"x-stainless-os":              "Linux",
			"x-stainless-arch":            "x64",
			"x-stainless-runtime":         "CPython",
			"x-stainless-runtime-version": "3.11.9",
			"anthropic-version":           "2023-06-01",
		},
	},
	"anthropic-typescript": {
		Name:        "anthropic-typescript",
		Description: "Official Anthropic TypeScript SDK",
		UserAgent:   "anthropic-typescript/0.38.0",
		ExtraHeader: HeaderSet{
			"x-stainless-lang":            "js",
			"x-stainless-package-version": "0.38.0",
			"x-stainless-os":              "Linux",
			"x-stainless-arch":            "x64",
			"x-stainless-runtime":         "node",
			"x-stainless-runtime-version": "v20.18.0",
			"anthropic-version":           "2023-06-01",
		},
	},
	"opencode": {
		Name:        "opencode",
		Description: "OpenCode Terminal AI Coding Agent (anomalyco/opencode)",
		UserAgent:   "opencode/1.18.2 (Linux x86_64; Bun/1.2.4)",
		ExtraHeader: HeaderSet{
			"x-opencode-version":          "1.18.2",
			"x-stainless-lang":            "js",
			"x-stainless-package-version": "1.18.2",
			"x-stainless-os":              "Linux",
			"x-stainless-arch":            "x64",
			"x-stainless-runtime":         "bun",
			"x-stainless-runtime-version": "1.2.4",
		},
	},
}

// BuiltinProfileNames returns the sorted/standard list of built-in profile names.
var BuiltinProfileNames = []string{
	"cursor",
	"claude-code",
	"cline",
	"roo-code",
	"windsurf",
	"opencode",
	"aider",
	"continue",
	"zed",
	"copilot",
	"openai-python",
	"openai-node",
	"anthropic-python",
	"anthropic-typescript",
}

// IsValidProfile reports whether name is a recognized built-in profile.
func IsValidProfile(name string) bool {
	_, ok := BuiltinProfiles[name]
	return ok
}

// RandomProfileName picks a random profile name from a list of candidates.
func RandomProfileName(candidates []string) string {
	if len(candidates) == 0 {
		candidates = BuiltinProfileNames
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(candidates))))
	if err != nil {
		return candidates[0]
	}
	return candidates[n.Int64()]
}
