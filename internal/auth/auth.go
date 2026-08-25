// Package auth implements client-key extraction and global validation.
//
// auth is a leaf package. ExtractKey applies the header-precedence rule:
// Authorization: Bearer takes precedence over x-api-key, which takes
// precedence over api-key. Header names and the Bearer scheme are matched
// case-insensitively, and values are trimmed. Validate checks the extracted
// key against the global client_keys set.
package auth

import "strings"

// ExtractKey returns the client key extracted from headers in precedence
// order:
//  1. Authorization: Bearer <token> (case-insensitive scheme; a missing or
//     malformed Bearer value falls through to the next header)
//  2. x-api-key (trimmed, non-empty)
//  3. api-key (trimmed, non-empty)
//
// ExtractKey returns an empty string when it finds no valid key. Header
// lookup is case-insensitive per RFC 9110. Values are trimmed of leading and
// trailing ASCII whitespace.
func ExtractKey(headers HeaderGetter) string {
	if v, ok := getHeaderCI(headers, "Authorization"); ok {
		if token, ok := BearerToken(v); ok {
			return token
		}
	}
	if v, ok := getHeaderCI(headers, "x-api-key"); ok {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	if v, ok := getHeaderCI(headers, "api-key"); ok {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// HeaderGetter is the minimal header interface. http.Header and test fakes
// satisfy it. Lookup is case-insensitive.
type HeaderGetter interface {
	Get(name string) string
}

// BearerToken parses a Bearer scheme value. "Bearer" is matched
// case-insensitively, followed by one or more SP or HTAB characters, then the
// token, which is trimmed. BearerToken returns ok=false when the scheme does
// not match or when no token remains after trimming.
func BearerToken(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if len(v) < len("Bearer") {
		return "", false
	}
	if !strings.EqualFold(v[:len("Bearer")], "Bearer") {
		return "", false
	}
	rest := v[len("Bearer"):]
	if rest == "" {
		return "", false // "Bearer" alone has no token.
	}
	// Require at least one SP or HTAB after the scheme.
	if rest[0] != ' ' && rest[0] != '\t' {
		return "", false // a non-Bearer scheme such as "BasicX"
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false // "Bearer    " has no token after trimming.
	}
	return token, true
}

// getHeaderCI is a case-insensitive Get for any HeaderGetter that follows
// http.Header semantics. http.Header.Get already performs a canonical
// case-insensitive lookup; getHeaderCI also handles custom getters that iterate
// keys.
func getHeaderCI(h HeaderGetter, name string) (string, bool) {
	// http.Header.Get already does canonical case-insensitive lookup.
	v := h.Get(name)
	if v != "" {
		return v, true
	}
	return "", false
}

// Validator is the global client_keys set.
type Validator struct {
	keys map[string]struct{}
}

// NewValidator builds a Validator from the global auth.client_keys list.
func NewValidator(clientKeys []string) *Validator {
	v := &Validator{keys: map[string]struct{}{}}
	for _, k := range clientKeys {
		v.keys[k] = struct{}{}
	}
	return v
}

// Validate reports whether the extracted client key is in the global set.
// The match is exact and case-sensitive.
func (v *Validator) Validate(clientKey string) bool {
	_, ok := v.keys[clientKey]
	return ok
}
