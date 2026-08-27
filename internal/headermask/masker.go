package headermask

import (
	"crypto/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Config configures header masking and spoofing.
type Config struct {
	// Mode determines how headers are masked:
	// - "passthrough" (or ""): leave headers untouched (except hop-by-hop & auth key).
	// - "random": randomize identifying headers on each request from Profiles (or all built-in).
	// - "profile": pin to a specific Profile.
	// - "custom": apply only CustomHeaders (with identifying client headers stripped).
	Mode string

	// Profile is the name of the pinned profile when Mode == "profile".
	Profile string

	// Profiles is the candidate pool of profile names when Mode == "random".
	// If empty, all BuiltinProfileNames are used.
	Profiles []string

	// CustomHeaders are explicit header key-value overrides applied on top.
	CustomHeaders map[string]string
}

// identifyingHeaderPrefixes lists header prefixes stripped when masking is enabled.
var identifyingHeaderPrefixes = []string{
	"x-stainless-",
	"x-cursor-",
	"x-client-",
	"x-aider-",
	"x-claude-",
	"x-codeium-",
	"x-opencode-",
}

// identifyingExactHeaders lists specific identifying headers stripped when masking is enabled.
var identifyingExactHeaders = []string{
	"user-agent",
	"editor-version",
	"editor-plugin-version",
	"x-amzn-trace-id",
}

// StripIdentifyingHeaders removes client fingerprinting headers from the request.
func StripIdentifyingHeaders(h http.Header) {
	for k := range h {
		lk := strings.ToLower(k)
		for _, exact := range identifyingExactHeaders {
			if lk == exact {
				h.Del(k)
				break
			}
		}
		for _, pfx := range identifyingHeaderPrefixes {
			if strings.HasPrefix(lk, pfx) {
				h.Del(k)
				break
			}
		}
	}
}

// Apply applies header masking and spoofing to the provided HTTP header map in-place.
func Apply(h http.Header, cfg Config) {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" || mode == "passthrough" || mode == "off" || mode == "none" {
		// Passthrough mode: no header masking or stripping.
		return
	}

	// 1. Strip identifying headers from incoming request.
	StripIdentifyingHeaders(h)

	var selectedProfile *Profile

	switch mode {
	case "random", "randomize":
		pool := cfg.Profiles
		if len(pool) == 0 {
			pool = BuiltinProfileNames
		}
		name := RandomProfileName(pool)
		if prof, ok := BuiltinProfiles[name]; ok {
			selectedProfile = &prof
		}
	case "profile", "preset", "select", "fixed":
		if prof, ok := BuiltinProfiles[cfg.Profile]; ok {
			selectedProfile = &prof
		} else if len(cfg.Profiles) > 0 {
			if p2, ok2 := BuiltinProfiles[cfg.Profiles[0]]; ok2 {
				selectedProfile = &p2
			}
		}
	case "custom":
		// Custom mode uses only CustomHeaders without an agent profile.
	}

	// 2. Inject authentic profile headers.
	if selectedProfile != nil {
		if selectedProfile.UserAgent != "" {
			h.Set("User-Agent", selectedProfile.UserAgent)
		}
		for k, v := range selectedProfile.ExtraHeader {
			h.Set(k, resolveHeaderValue(v))
		}
	}

	// 3. Apply custom header overrides on top.
	for k, v := range cfg.CustomHeaders {
		if strings.TrimSpace(k) != "" {
			h.Set(k, resolveHeaderValue(v))
		}
	}
}

// resolveHeaderValue substitutes dynamic sentinel values in a profile or
// custom header value.
func resolveHeaderValue(v string) string {
	if v == TaskIDPlaceholder {
		return newTaskID()
	}
	return v
}

// taskIDAlphabet is nanoid's default alphabet (64 chars, so byte%64 is uniform).
const taskIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_-"

// newTaskID generates an identifier matching Cline's session id format
// (<unix-millis>_<5-char nanoid>), used as the X-Task-ID header value.
func newTaskID() string {
	suffix := make([]byte, 5)
	if _, err := rand.Read(suffix); err != nil {
		return strconv.FormatInt(time.Now().UnixMilli(), 10) + "_00000"
	}
	for i := range suffix {
		suffix[i] = taskIDAlphabet[int(suffix[i])%len(taskIDAlphabet)]
	}
	return strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + string(suffix)
}
