package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestRegistryPrometheus(t *testing.T) {
	r := New(time.Unix(1000, 0))
	r.RecordRequest("openai", "gpt-4o", 200)
	r.RecordRequest("openai", "gpt-4o", 200)
	r.RecordRequest("openai", "gpt-4o", 500)
	r.RecordError("openai", "UPSTREAM_OUTAGE")
	r.RecordError("openai", "KEY_AUTH_FATAL")
	r.RecordDuration("openai", 5)
	r.RecordDuration("openai", 50)
	r.SetKeysGauge("openai", 4, 1, 1, 0)
	r.SetProxyHealthy("http://p:8080", true)
	r.SetProxyHealthy("http://q:8080", false)

	var sb strings.Builder
	r.WritePrometheus(&sb)
	out := sb.String()

	checks := []string{
		"conflux_uptime_seconds",
		"conflux_requests_total 3",
		`conflux_requests_by_provider{provider="openai",status="200"} 2`,
		`conflux_requests_by_provider{provider="openai",status="500"} 1`,
		`conflux_requests_by_model{model="gpt-4o",provider="openai",status="200"} 2`,
		`conflux_error_categories_total{provider="openai",category="UPSTREAM_OUTAGE"} 1`,
		`conflux_error_categories_total{provider="openai",category="KEY_AUTH_FATAL"} 1`,
		"# TYPE conflux_request_duration_ms histogram",
		`conflux_request_duration_ms_count{provider="openai"} 2`,
		`conflux_keys{provider="openai",state="active"} 4`,
		`conflux_keys{provider="openai",state="standby"} 1`,
		`conflux_proxy_healthy{proxy="http://p:8080"} 1`,
		`conflux_proxy_healthy{proxy="http://q:8080"} 0`,
	}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("missing %q in output:\n%s", c, out)
		}
	}
}

func TestHistogramCumulative(t *testing.T) {
	r := New(time.Unix(0, 0))
	// Observe the values 1, 10, 100, and 1000.
	r.RecordDuration("p", 1)
	r.RecordDuration("p", 10)
	r.RecordDuration("p", 100)
	r.RecordDuration("p", 1000)
	var sb strings.Builder
	r.WritePrometheus(&sb)
	out := sb.String()
	// le="1" should have 1, only the 1ms observation; le="10" should have 2;
	// le="100" should have 3; le="1000" should have 4.
	if !strings.Contains(out, `conflux_request_duration_ms_bucket{provider="p",le="1"} 1`) {
		t.Errorf("le=1 wrong:\n%s", out)
	}
	if !strings.Contains(out, `conflux_request_duration_ms_bucket{provider="p",le="10"} 2`) {
		t.Errorf("le=10 wrong:\n%s", out)
	}
	if !strings.Contains(out, `conflux_request_duration_ms_bucket{provider="p",le="100"} 3`) {
		t.Errorf("le=100 wrong:\n%s", out)
	}
	if !strings.Contains(out, `conflux_request_duration_ms_count{provider="p"} 4`) {
		t.Errorf("count wrong:\n%s", out)
	}
}

func TestStatus(t *testing.T) {
	r := New(time.Unix(1000, 0))
	r.RecordRequest("openai", "gpt-4o", 200)
	detail := StatusDetail{
		GlobalProxies: []string{"http://p:8080"},
		ClientKeys:    []string{"sk-…-001"},
		Providers:     map[string]any{"openai": map[string]any{"baseUrl": "https://api.openai.com/v1"}},
	}
	ph := map[string]ProxyHealth{"http://p:8080": {Healthy: true, ConsecutiveErrors: 0}}
	s := r.Status("3.0", detail, ph)
	if s.Version != "3.0" {
		t.Errorf("version = %q", s.Version)
	}
	if !s.OK {
		t.Error("ok should be true")
	}
	if s.Metrics.TotalRequests != 1 {
		t.Errorf("totalRequests = %d", s.Metrics.TotalRequests)
	}
	if s.Metrics.TotalErrors != 0 {
		t.Errorf("totalErrors = %d, want 0 (the single response was 200)", s.Metrics.TotalErrors)
	}
	if len(s.Status.ClientKeys) != 1 || s.Status.ClientKeys[0] != "sk-…-001" {
		t.Errorf("clientKeys = %v", s.Status.ClientKeys)
	}
}

// TestResponseErrors verifies that TotalErrors is response-based (one per
// downstream response with status >= 400), NOT the per-attempt error-category
// count. Per-attempt categories (retries, in-stream errors) are a separate
// series; the /_status headline error count must pair with requestsTotal so
// success/error rates stay in [0,100] and never go negative.
func TestResponseErrors(t *testing.T) {
	r := New(time.Unix(1000, 0))
	// 3 downstream responses: 200, 500, 502.
	r.RecordRequest("openai", "gpt-4o", 200)
	r.RecordRequest("openai", "gpt-4o", 500)
	r.RecordRequest("openai", "gpt-4o", 502)
	// Per-attempt error categories: the 500 was retried once, and an auth-fatal
	// happened on a second attempt. These are NOT response counts.
	r.RecordError("openai", "UPSTREAM_OUTAGE")
	r.RecordError("openai", "KEY_AUTH_FATAL")
	detail := StatusDetail{Providers: map[string]any{}}
	s := r.Status("3.0", detail, nil)
	if s.Metrics.TotalRequests != 3 {
		t.Errorf("totalRequests = %d, want 3", s.Metrics.TotalRequests)
	}
	if s.Metrics.TotalErrors != 2 {
		t.Errorf("totalErrors = %d, want 2 (the 500 and 502 responses, not the 2 per-attempt categories)", s.Metrics.TotalErrors)
	}

	snap := r.Snapshot()
	if snap.ResponseErrors != 2 {
		t.Errorf("Snapshot.ResponseErrors = %d, want 2", snap.ResponseErrors)
	}
}
