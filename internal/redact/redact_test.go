package redact

import (
	"net/http"
	"testing"
)

func TestKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sk-conflux-global-001", "sk-…-001"}, // 21 chars: first3 + last4
		{"sk-ant-api03-xyzabcd1234", "sk-…1234"},
		{"ghp_abcdef0123456789", "ghp…6789"},
		{"shortkey", "sh…ey"}, // 8 chars: first2 + last2
		{"12345678", "12…78"},
		{"1234567", "****"}, // 7 chars: ****
		{"abc", "****"},
		{"", "****"},
		{"  sk-conflux-global-001  ", "sk-…-001"}, // trimmed
	}
	for _, c := range cases {
		if got := Key(c.in); got != c.want {
			t.Errorf("Key(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAuthHeader(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Bearer sk-conflux-global-001", "Bearer sk-…-001"},
		{"bearer sk-ant-api03-xyzabcd1234", "Bearer sk-…1234"}, // scheme is case-insensitive
		{"Bearer    sk-ant-api03-xyzabcd1234   ", "Bearer sk-…1234"},
		{"Bearer", "****"},                     // no token: mask the whole value
		{"Basic abcdefghij123456", "Bas…3456"}, // non-Bearer, 22 chars: first3 + last4
		{"sk-raw-key-abcdefghij", "sk-…ghij"},
	}
	for _, c := range cases {
		if got := AuthHeader(c.in); got != c.want {
			t.Errorf("AuthHeader(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-conflux-global-001")
	h.Set("X-Api-Key", "sk-other-abcdefghij")
	h.Set("Proxy-Authorization", "Basic dXNlcjpwYXNz")
	h.Set("Content-Type", "application/json")
	out := Headers(h)
	if out.Get("Authorization") != "Bearer sk-…-001" {
		t.Errorf("Authorization = %q", out.Get("Authorization"))
	}
	if out.Get("X-Api-Key") == "sk-other-abcdefghij" {
		t.Error("X-Api-Key should be masked")
	}
	if out.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should be preserved")
	}
}

func TestURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://user:pass@host:8080", "http://host:8080"},
		{"http://host:8080", "http://host:8080"},
		{"socks5://u:p@127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"not a url", "not a url"},
		{"", ""},
	}
	for _, c := range cases {
		if got := URL(c.in); got != c.want {
			t.Errorf("URL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQueryValues(t *testing.T) {
	cases := []struct{ in, want string }{
		// Sensitive value masked, other params and order preserved.
		{"https://api.x.com/v1/chat?key=secret&model=gpt-4o", "https://api.x.com/v1/chat?key=****&model=gpt-4o"},
		// Case-insensitive key match.
		{"https://x.com/c?Key=secret", "https://x.com/c?Key=****"},
		{"https://x.com/c?API_KEY=secret", "https://x.com/c?API_KEY=****"},
		// Multiple sensitive keys in one query.
		{"https://x.com/c?token=t1&api_key=a1&safe=s", "https://x.com/c?token=****&api_key=****&safe=s"},
		// No sensitive params: unchanged.
		{"https://x.com/c?model=m&n=1", "https://x.com/c?model=m&n=1"},
		// No query: unchanged.
		{"https://x.com/c", "https://x.com/c"},
		// A key-like substring that is not a param boundary is not masked.
		{"https://x.com/apikey=foo", "https://x.com/apikey=foo"},
	}
	for _, c := range cases {
		if got := QueryValues(c.in); got != c.want {
			t.Errorf("QueryValues(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
