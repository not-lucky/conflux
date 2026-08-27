package forward

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/not-lucky/conflux/internal/auth"
	"github.com/not-lucky/conflux/internal/headermask"
)

// rewrite applies the URL construction, header substitution, hop-by-hop strip,
// and fallback model re-serialization. It returns the UpstreamRequest, the
// effective model after any fallback, whether a fallback rewrite occurred,
// and any error.
//
// clientKey is the extracted client key used for header substitution
// comparison; the server stamps it into a transient header that rewrite
// strips.
func (f *Forwarder) rewrite(req *Request, ph ProviderHandle, sel Selection, psel ProxySelection) (*UpstreamRequest, string, bool, error) {
	policy := ph.Policy()
	h := req.Headers.Clone()
	stripHopByHop(h)

	// The server stamps the extracted client key here so rewrite can compare it
	// for header substitution; rewrite strips it before forwarding.
	clientKey := h.Get("X-Conflux-Client-Key")
	h.Del("X-Conflux-Client-Key")

	providerKey := sel.Key
	substituteHeaders(h, clientKey, providerKey)

	// Apply header masking and agent spoofing/randomization.
	// If the selected key has an inline profile override, pin that profile.
	maskCfg := policy.HeaderMasking
	if sel.Profile != "" {
		maskCfg.Mode = "profile"
		maskCfg.Profile = sel.Profile
	}
	headermask.Apply(h, maskCfg)

	// URL construction.
	upURL, err := buildUpstreamURL(policy.BaseURL, req.Path, req.RawQuery)
	if err != nil {
		return nil, "", false, err
	}

	// Body and fallback model rewrite.
	body := req.Body
	effModel := req.Model
	rewrote := false
	if len(policy.FallbackModels) > 0 {
		if to, ok := policy.FallbackModels[req.Model]; ok {
			newBody, did, rerr := rewriteModelField(body, req.Model, to)
			if rerr == nil && did {
				body = newBody
				effModel = to
				rewrote = true
				h.Set("Content-Type", "application/json")
				h.Set("Content-Length", strconv.Itoa(len(body)))
				h.Del("Content-Encoding")
			}
		}
	}

	upReq := &UpstreamRequest{
		Method:  req.Method,
		URL:     upURL,
		Headers: h,
		Body:    body,
	}
	return upReq, effModel, rewrote, nil
}

// substituteHeaders replaces the client key with the provider key in the
// candidate headers: Authorization, x-api-key, and api-key. Candidate header
// values that exactly equal the client key are replaced; remaining candidate
// values that do not equal the client key are stripped.
func substituteHeaders(h http.Header, clientKey, providerKey string) {
	candidates := []string{"Authorization", "X-Api-Key", "Api-Key"}
	matched := false
	for _, name := range candidates {
		vals := h.Values(name)
		if len(vals) == 0 {
			continue
		}
		kept := vals[:0]
		for _, v := range vals {
			if headerValueMatches(v, name, clientKey) {
				matched = true
				if name == "Authorization" {
					kept = append(kept, "Bearer "+providerKey)
				} else {
					kept = append(kept, providerKey)
				}
			}
			// A non-matching candidate value is stripped.
		}
		if len(kept) == 0 {
			h.Del(name)
		} else {
			h[name] = kept
		}
	}
	// When no candidate matched exactly, set Authorization: Bearer <providerKey>
	// for completeness.
	if !matched && providerKey != "" {
		h.Set("Authorization", "Bearer "+providerKey)
	}
}

// headerValueMatches reports whether a candidate header value equals the
// client key. For Authorization, the token part after Bearer is compared.
func headerValueMatches(v, headerName, clientKey string) bool {
	v = strings.TrimSpace(v)
	if headerName == "Authorization" {
		token, ok := auth.BearerToken(v)
		if !ok {
			return false
		}
		return token == clientKey
	}
	return strings.TrimSpace(v) == clientKey
}

// buildUpstreamURL constructs the upstream URL from the base_url, whose
// trailing slash is already stripped, plus the client path with first-segment
// dedup, plus the search part.
func buildUpstreamURL(base, clientPath, rawQuery string) (string, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	// Single-segment dedup: when the base first segment equals the client first
	// segment, drop the client's first segment once.
	bp := strings.Trim(bu.Path, "/")
	cp := strings.Trim(clientPath, "/")
	baseSegs := splitNonEmpty(bp, "/")
	clientSegs := splitNonEmpty(cp, "/")
	if len(baseSegs) > 0 && len(clientSegs) > 0 && baseSegs[0] == clientSegs[0] {
		clientSegs = clientSegs[1:]
	}
	final := strings.Join(baseSegs, "/")
	if len(clientSegs) > 0 {
		if final != "" {
			final += "/"
		}
		final += strings.Join(clientSegs, "/")
	}
	bu.Path = "/" + final
	bu.RawQuery = rawQuery
	return bu.String(), nil
}

func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, sep)
}

// rewriteModelField rewrites the top-level JSON `model` field from to to,
// and re-serializes the body. It returns the new body, whether a rewrite
// occurred, and any error. Non-JSON bodies are left untouched, in which case
// did is false.
func rewriteModelField(body []byte, from, to string) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false, nil // not JSON, so left untouched
	}
	m, ok := obj["model"]
	if !ok {
		return body, false, nil
	}
	ms, ok := m.(string)
	if !ok || ms != from {
		return body, false, nil
	}
	obj["model"] = to
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}
