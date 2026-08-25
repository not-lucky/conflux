// Package config loads, validates, and resolves the Conflux operator
// configuration.
//
// config is a leaf package: it imports only the standard library and a YAML
// decoder. Its single interface is Load(path) (*Config, error). The returned
// Config has all validation applied and all effective values baked in, so
// there is no runtime inheritance lookup. Every other package reads a fully
// resolved tree.
package config

import (
	"errors"
	"fmt"
)

// Typed validation errors. Callers use errors.Is to distinguish the required
// distinct error messages, such as duration syntax versus positivity.
var (
	ErrUnknownKey          = errors.New("unknown config key")
	ErrBadDuration         = errors.New("invalid duration")
	ErrDurationNotPositive = errors.New("duration must be >0")
	ErrInvalidModel        = errors.New("invalid model entry")
	ErrInvalidProxyURL     = errors.New("invalid proxy url")
	ErrInvalidBaseURL      = errors.New("invalid base_url")
	ErrInvalidPort         = errors.New("invalid server.port")
	ErrDuplicateKey        = errors.New("duplicate provider key")
	ErrDuplicateModel      = errors.New("duplicate model entry")
	ErrAuthError           = errors.New("auth config error")
	ErrProviderError       = errors.New("provider config error")
	ErrActiveWindowError   = errors.New("active_window error")
	ErrRetryError          = errors.New("retry config error")
	ErrProxyShape          = errors.New("proxies shape error")
	ErrFallbackError       = errors.New("fallback_models error")
)

// fieldError wraps a sentinel with human-readable field context.
type fieldError struct {
	err   error
	field string
	msg   string
}

func (e *fieldError) Error() string {
	if e.field != "" {
		return e.field + ": " + e.msg
	}
	return e.msg
}

func (e *fieldError) Unwrap() error { return e.err }

func wrapf(err error, field, format string, args ...any) error {
	return &fieldError{err: err, field: field, msg: fmt.Sprintf(format, args...)}
}

// wrap attaches a plain, non-format message to a sentinel error. Use wrap
// when the message is a literal or a user-provided value that must not be
// interpreted as a format string.
func wrap(err error, field, msg string) error {
	return &fieldError{err: err, field: field, msg: msg}
}
