package config

import (
	"strings"
)

// parseModelEntry validates one `models` entry and returns its parsed form.
// The rules are:
//   - non-empty, no whitespace
//   - ":" is allowed (model ids such as "nvidia/nemotron-3.5-lightning:free"
//     carry an OpenRouter free-tier suffix); it is not a delimiter here
//   - "*" only as a trailing suffix or standalone
//   - "*foo", "a*b", "a *", and "" are errors
//
// Entries are stored trimmed, so parseModelEntry trims the edges.
func parseModelEntry(s string) (ModelEntry, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ModelEntry{}, wrapf(ErrInvalidModel, "", "empty model entry after trim")
	}
	if strings.ContainsAny(s, " \t") {
		return ModelEntry{}, wrapf(ErrInvalidModel, "", "model entry %q contains whitespace", s)
	}
	if s == "*" {
		return ModelEntry{Kind: ModelCatchAll, Literal: "*"}, nil
	}
	// A trailing * means a prefix match. A leading * ("*foo") or a middle *
	// ("a*b") is an error.
	if strings.HasSuffix(s, "*") {
		prefix := strings.TrimSuffix(s, "*")
		if prefix == "" {
			// This should not happen because s=="*" is handled above, but guard
			// against it anyway.
			return ModelEntry{}, wrapf(ErrInvalidModel, "", "model entry %q invalid", s)
		}
		if strings.ContainsRune(prefix, '*') {
			return ModelEntry{}, wrapf(ErrInvalidModel, "", "model entry %q has '*' not at end", s)
		}
		return ModelEntry{Kind: ModelPrefix, Literal: prefix}, nil
	}
	// No trailing *; a stray * anywhere is invalid.
	if strings.ContainsRune(s, '*') {
		return ModelEntry{}, wrapf(ErrInvalidModel, "", "model entry %q has '*' not at end", s)
	}
	return ModelEntry{Kind: ModelExact, Literal: s}, nil
}
