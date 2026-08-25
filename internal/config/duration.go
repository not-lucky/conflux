package config

import (
	"regexp"
	"time"
)

// durRe is the duration syntax: ^[0-9]+(ms|s|m|h)$. The positivity check
// is a separate semantic step so the two error messages stay distinct.
var durRe = regexp.MustCompile(`^[0-9]+(ms|s|m|h)$`)

// parseDuration validates and parses a duration string. It returns a
// distinct error for syntax (ErrBadDuration) versus a non-positive value
// (ErrDurationNotPositive).
func parseDuration(s string) (time.Duration, error) {
	if !durRe.MatchString(s) {
		return 0, ErrBadDuration
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		// This should be unreachable given the regex, but keep the error typed.
		return 0, ErrBadDuration
	}
	if d <= 0 {
		return 0, ErrDurationNotPositive
	}
	return d, nil
}
