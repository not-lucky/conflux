// Package redact masks secrets for logs, traces, metrics, and /_status.
//
// redact imports internal/auth for the canonical Bearer-token parser. The
// masking rules are normative: the mask derives from the actual key value, a
// real prefix plus the last 4 characters, not a fixed template, so different
// key families stay distinguishable.
package redact

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/not-lucky/conflux/internal/auth"
)

// Key masks a raw secret:
//   - len >= 20: first3 + "…" + last4
//   - len >= 8:  first2 + "…" + last2
//   - else:      "****"
//
// The token-type prefix, such as sk-, sk-ant-, or ghp_, is preserved verbatim.
func Key(v string) string {
	v = strings.TrimSpace(v)
	switch {
	case len(v) >= 20:
		return v[:3] + "…" + v[len(v)-4:]
	case len(v) >= 8:
		return v[:2] + "…" + v[len(v)-2:]
	default:
		return "****"
	}
}

// AuthHeader masks an Authorization header value. When the value is a Bearer
// scheme, the token part is masked and the canonical "Bearer " prefix is
// re-prepended (scheme matched case-insensitively via auth.BearerToken).
// Otherwise the whole value is masked as a Key.
func AuthHeader(v string) string {
	if tok, ok := auth.BearerToken(strings.TrimSpace(v)); ok {
		return "Bearer " + Key(tok)
	}
	return Key(v)
}

// URL strips credentials from a proxy or URL string: "http://user:pass@h:8080"
// becomes "http://h:8080". When parsing fails, the input is returned unchanged,
// best-effort and never panicking on malformed input.
func URL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return s
	}
	u.User = nil
	return u.String()
}

// queryKeys is the set of query parameter names whose values are masked in
// QueryValues. Matched case-insensitively against the decoded key.
var queryKeys = map[string]bool{
	"key":     true,
	"api_key": true,
	"token":   true,
}

// QueryValues masks the values of sensitive query parameters (key, api_key,
// token) in a URL string, replacing each with "****" while preserving the
// rest of the URL verbatim — scheme, host, path, other params, and their
// original order. It uses net/url to locate the query and to decode each key
// so masking is robust against URL-encoding and parameter ordering, unlike a
// raw substring search. On any parse failure the input is returned unchanged.
func QueryValues(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	rq := u.RawQuery
	if rq == "" {
		return s
	}
	parts := strings.Split(rq, "&")
	for i, p := range parts {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			continue
		}
		k, err := url.QueryUnescape(p[:eq])
		if err != nil {
			continue
		}
		if queryKeys[strings.ToLower(k)] {
			parts[i] = p[:eq+1] + "****"
		}
	}
	u.RawQuery = strings.Join(parts, "&")
	return u.String()
}

// Headers returns a copy of h with sensitive headers (authorization,
// x-api-key, api-key, proxy-authorization) redacted via AuthHeader or Key.
// Other headers are preserved verbatim. Header names are matched
// case-insensitively.
func Headers(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := h.Clone()
	for _, name := range []string{"Authorization", "X-Api-Key", "Api-Key", "Proxy-Authorization"} {
		vals := out.Values(name)
		if len(vals) == 0 {
			continue
		}
		for i, v := range vals {
			if name == "Authorization" {
				vals[i] = AuthHeader(v)
			} else {
				vals[i] = Key(v)
			}
		}
		out[name] = vals
	}
	return out
}
