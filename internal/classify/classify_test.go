package classify

import "testing"

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Category
		penal  bool
		retry  bool
	}{
		{200, "", Success, false, false},
		{204, "", Success, false, false},
		{301, "", Redirect, false, false},
		{308, "", Redirect, false, false},
		{400, `{"error":"bad"}`, ClientError, false, false},
		{404, `{"error":"model not found"}`, ClientError, false, false},
		{422, `{}`, ClientError, false, false},
		{413, ``, ClientError, false, false},
		{401, `{"error":{"message":"invalid api key"}}`, KeyAuthFatal, true, true},
		{403, ``, KeyAuthFatal, true, true},
		{402, `{"error":"billing required"}`, KeyBilling, true, true},
		{500, ``, UpstreamOutage, false, true},
		{503, ``, UpstreamOutage, false, true},
		{599, ``, UpstreamOutage, false, true},
		{100, ``, UnknownError, false, false},
		{600, ``, UnknownError, false, false},
	}
	for i, c := range cases {
		got := Classify(Response{Status: c.status, Body: []byte(c.body)})
		if got.Category != c.want || got.Penalize != c.penal || got.Retryable != c.retry {
			t.Errorf("case %d (status %d): got %s penal=%v retry=%v, want %s penal=%v retry=%v",
				i, c.status, got.Category, got.Penalize, got.Retryable, c.want, c.penal, c.retry)
		}
	}
}

func TestClassify429(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Category
	}{
		// Any 429 is treated as KeyRateLimited
		{"openai rate_limit_exceeded", `{"error":{"type":"rate_limit_exceeded"}}`, KeyRateLimited},
		{"openai quota", `{"error":{"message":"insufficient_quota"}}`, KeyRateLimited},
		{"anthropic overloaded", `{"error":{"type":"overloaded"}}`, KeyRateLimited},
		{"per-key phrase", `{"error":{"message":"your api key rate limit reached"}}`, KeyRateLimited},
		{"generic 429", `{"error":{"message":"slow down"}}`, KeyRateLimited},
		{"empty body", ``, KeyRateLimited},
		{"unparseable", `not json`, KeyRateLimited},
	}
	for _, c := range cases {
		got := Classify(Response{Status: 429, Body: []byte(c.body)})
		if got.Category != c.want || !got.Penalize || !got.Retryable {
			t.Errorf("case %q: got %+v, want %s (penalize=true, retryable=true)", c.name, got, c.want)
		}
	}
}

func TestClassifyTransport(t *testing.T) {
	msgs := []string{
		"connect ECONNREFUSED 1.2.3.4:443",
		"socket hang up",
		"failed to fetch",
		"network error",
		"timeout",
		"socks proxy error",
		"ETIMEDOUT",
	}
	for _, m := range msgs {
		got := Classify(Response{TransportErr: m})
		if got.Category != ProxyNetworkError || got.Penalize || !got.Retryable {
			t.Errorf("transport %q: got %s penal=%v retry=%v, want PROXY_NETWORK_ERROR no-penalize retryable", m, got.Category, got.Penalize, got.Retryable)
		}
	}
	// A TTFB expiry reported via the sentinel.
	if got := Classify(Response{TransportErr: "ttfb timeout"}); got.Category != ProxyNetworkError {
		t.Errorf("ttfb: got %s", got.Category)
	}
}

func TestParseSSEPayload(t *testing.T) {
	cases := []struct {
		payload string
		isErr   bool
	}{
		{`{"error":{"message":"oops"}}`, true},
		{`{"type":"error","error":{"message":"x"}}`, true},
		{`{"content":"hello"}`, false},
		{`[DONE]`, false},
		{``, false},
		{`not json`, false},
		// Error inside completion text: {"content":"Error 401 in story"} is not an
		// envelope.
		{`{"content":"Error 401 in story"}`, false},
		// But {"error":{"message":"..."}} anywhere in a data line is treated as
		// an error.
		{`{"error":{"message":"x"},"content":"..."}`, true},
	}
	for _, c := range cases {
		_, isErr := ParseSSEPayload(c.payload)
		if isErr != c.isErr {
			t.Errorf("ParseSSEPayload(%q) isErr=%v, want %v", c.payload, isErr, c.isErr)
		}
	}
}

func TestClassifySSE(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    Category
	}{
		{"key-specific 429", `{"error":{"type":"rate_limit_exceeded"}}`, KeyRateLimited},
		{"auth-fatal status", `{"error":{"status":401,"message":"invalid key"}}`, KeyAuthFatal},
		{"auth-fatal type", `{"error":{"type":"authentication_error"}}`, KeyAuthFatal},
		{"permission error", `{"error":{"type":"permission_error"}}`, KeyAuthFatal},
		{"shared rate limit", `{"error":{"message":"rate limit shared"}}`, KeyRateLimited},
		{"shared 429 status", `{"error":{"status":429,"message":"too many requests"}}`, KeyRateLimited},
		{"shared 429 code string", `{"error":{"code":"429","message":"slow down"}}`, KeyRateLimited},
		{"generic error", `{"error":{"message":"internal"}}`, UpstreamOutage},
	}
	for _, c := range cases {
		obj, isErr := ParseSSEPayload(c.payload)
		if !isErr {
			t.Fatalf("case %q: expected error envelope", c.name)
		}
		res := ClassifySSE(obj)
		if !res.IsError || res.Category != c.want {
			t.Errorf("case %q: got %s, want %s", c.name, res.Category, c.want)
		}
	}
}

func TestCategoryIsError(t *testing.T) {
	if Success.IsError() {
		t.Error("Success should not be an error")
	}
	if Redirect.IsError() {
		t.Error("Redirect should not be an error")
	}
	if !SharedPoolRateLimited.IsError() {
		t.Error("SharedPoolRateLimited should be marked as an error")
	}
	if !KeyRateLimited.IsError() {
		t.Error("KeyRateLimited should be marked as an error")
	}
	if !UpstreamOutage.IsError() {
		t.Error("UpstreamOutage should be marked as an error")
	}
}
