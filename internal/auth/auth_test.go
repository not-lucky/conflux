package auth

import (
	"net/http"
	"testing"
)

func TestExtractKeyPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		want    string
	}{
		{"bearer", headers("Authorization", "Bearer sk-global-001"), "sk-global-001"},
		{"bearer case-insensitive", headers("Authorization", "bearer sk-global-001"), "sk-global-001"},
		{"bearer with spaces", headers("Authorization", "Bearer    sk-global-001   "), "sk-global-001"},
		{"x-api-key", headers("x-api-key", "sk-xkey"), "sk-xkey"},
		{"api-key fallback", headers("api-key", "sk-akey"), "sk-akey"},
		{"bearer wins over x-api-key", multi(headers("Authorization", "Bearer sk-bearer"), headers("x-api-key", "sk-xkey")), "sk-bearer"},
		{"bearer malformed falls to x-api-key", multi(headers("Authorization", "Basic abc"), headers("x-api-key", "sk-xkey")), "sk-xkey"},
		{"bearer alone missing token falls to x-api-key", multi(headers("Authorization", "Bearer"), headers("x-api-key", "sk-xkey")), "sk-xkey"},
		{"none", http.Header{}, ""},
		{"header name case-insensitive", headers("X-API-KEY", "sk-xkey"), "sk-xkey"},
	}
	for _, c := range cases {
		got := ExtractKey(c.headers)
		if got != c.want {
			t.Errorf("case %q: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidator(t *testing.T) {
	v := NewValidator([]string{"sk-1", "sk-2"})
	if !v.Validate("sk-1") {
		t.Error("sk-1 should validate")
	}
	if v.Validate("sk-unknown") {
		t.Error("unknown should not validate")
	}
	if v.Validate("SK-1") {
		t.Error("case-sensitive: SK-1 should not validate")
	}
}

func headers(k, v string) http.Header {
	h := http.Header{}
	h.Set(k, v)
	return h
}

func multi(hs ...http.Header) http.Header {
	out := http.Header{}
	for _, h := range hs {
		for k, vs := range h {
			for _, v := range vs {
				out.Add(k, v)
			}
		}
	}
	return out
}
