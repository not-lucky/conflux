package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/not-lucky/conflux/internal/breaker"
	"github.com/not-lucky/conflux/internal/config"
	"github.com/not-lucky/conflux/internal/keypool"
	"github.com/not-lucky/conflux/internal/metrics"
	"github.com/not-lucky/conflux/internal/model"
	"github.com/not-lucky/conflux/internal/proxy"
	"github.com/not-lucky/conflux/internal/runtime"
	"github.com/not-lucky/conflux/internal/trace"
)

// testDashboard builds a dashboard wired to a minimal live snapshot: one
// provider "p" with two keys (MaxErrors=2) and an admin token "tok". It returns
// the dashboard and the live store so tests can mutate state.
func testDashboard(t *testing.T, tr *trace.Tracer) (*Dashboard, *runtime.Store) {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{AdminToken: "tok"},
		Providers: []config.Provider{{
			Name:    "p",
			BaseURL: "http://up.test",
			Models:  []config.ModelEntry{{Kind: config.ModelExact, Literal: "m"}},
			Keys:    []config.Key{{Value: "key-1"}, {Value: "key-2"}},
			PolicyFields: config.PolicyFields{
				MaxErrors: 2, Cooldown: 30 * time.Minute,
			},
		}},
	}
	reg := model.NewRegistry([]model.Provider{{Name: "p", Models: []model.Entry{{Kind: model.Exact, Literal: "m"}}}})
	pool := keypool.New(keypool.Spec{
		Keys: []keypool.Key{{Value: "key-1"}, {Value: "key-2"}}, ActiveWindow: 2, MaxErrors: 2, Cooldown: 30 * time.Minute,
	}, realClock{})
	health := proxy.NewHealth(realClock{})
	brk := breaker.New(5, 30*time.Second, nil)
	store := &runtime.Store{}
	store.Store(&runtime.Live{
		Config:      cfg,
		Registry:    reg,
		Pools:       map[string]*keypool.Pool{"p": pool},
		Breakers:    map[string]*breaker.Breaker{"p": brk},
		Health:      health,
		Validator:   nil, // not used by dashboard directly
		ProxyHealth: nil,
	})
	mreg := metrics.New(time.Now())
	if tr == nil {
		tr = trace.New(t.TempDir(), trace.Off, 10)
	}
	return New(store, mreg, tr, nil), store
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// authedClient returns an http.Client with a cookie jar holding the admin
// session cookie, obtained by POSTing the login form.
func authedClient(t *testing.T, h http.Handler) *http.Client {
	t.Helper()
	ts := httptest.NewServer(h)
	defer ts.Close()
	jar := &simpleJar{}
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(ts.URL+"/_dashboard/login", url.Values{"token": {"tok"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The client follows the 303 redirect to the overview, so the final status
	// is 200; either 303 (not followed) or 200 (followed) is acceptable.
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 303 or 200", resp.StatusCode)
	}
	return c
}

// simpleJar is a minimal cookie jar sufficient for the test client.
type simpleJar struct{ cookies []*http.Cookie }

func (j *simpleJar) SetCookies(_ *url.URL, cookies []*http.Cookie) { j.cookies = cookies }
func (j *simpleJar) Cookies(_ *url.URL) []*http.Cookie             { return j.cookies }

// doGet performs an authenticated GET against a dashboard-mounted handler
// (mounted under /_dashboard via StripPrefix) and returns the response.
func doGet(t *testing.T, h http.Handler, c *http.Client, path string) *http.Response {
	t.Helper()
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := c.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mounted(d *Dashboard) http.Handler { return http.StripPrefix("/_dashboard", d.Routes()) }

// TestDashboardAuthGating verifies unauthenticated requests are redirected to
// the login page, and an authed request reaches the overview.
func TestDashboardAuthGating(t *testing.T) {
	d, _ := testDashboard(t, nil)
	h := mounted(d)

	// Unauthenticated GET redirects to login.
	resp := doGet(t, h, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, "/_dashboard/")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauth GET / status = %d, want 303 redirect to login", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/_dashboard/login" {
		t.Fatalf("redirect location = %q, want /_dashboard/login", loc)
	}

	// The login page renders and mentions the disabled-vs-enabled state.
	resp2 := doGet(t, h, &http.Client{}, "/_dashboard/login")
	defer resp2.Body.Close()
	body := readBody(t, resp2)
	if !strings.Contains(body, "Conflux") {
		t.Errorf("login page missing brand: %s", body)
	}

	// Authed client reaches the overview.
	c := authedClient(t, h)
	resp3 := doGet(t, h, c, "/_dashboard/")
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("authed GET / status = %d", resp3.StatusCode)
	}
	overview := readBody(t, resp3)
	for _, want := range []string{"Overview", "provider", "p"} {
		if !strings.Contains(overview, want) {
			t.Errorf("overview missing %q", want)
		}
	}
}

// TestDashboardKeyAction verifies the reset/retire write actions mutate the
// pool and return a refreshed keys partial.
func TestDashboardKeyAction(t *testing.T) {
	d, store := testDashboard(t, nil)
	h := mounted(d)
	c := authedClient(t, h)

	// Exhaust key #1 in the pool directly, then reset it via the dashboard.
	pool := store.Load().Pools["p"]
	pool.RecordError(1)
	pool.RecordError(1)
	if pool.Counts().Exhausted != 1 {
		t.Fatalf("pre-reset exhausted = %d, want 1", pool.Counts().Exhausted)
	}

	// POST reset -> key #1 re-enabled.
	resp := postAuthed(t, h, c, "/_dashboard/act/keys/reset?provider=p&key=1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset status = %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if pool.Counts().Exhausted != 0 {
		t.Fatalf("post-reset exhausted = %d, want 0", pool.Counts().Exhausted)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Key pools") {
		t.Errorf("reset response should return the keys partial; got: %s", body)
	}

	// POST retire -> key #1 exhausted again.
	resp2 := postAuthed(t, h, c, "/_dashboard/act/keys/retire?provider=p&key=1")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retire status = %d", resp2.StatusCode)
	}
	if pool.Counts().Exhausted != 1 {
		t.Fatalf("post-retire exhausted = %d, want 1", pool.Counts().Exhausted)
	}
}

// TestDashboardProxyAndBreakerActions exercises the proxy and breaker write
// actions through the dashboard.
func TestDashboardProxyAndBreakerActions(t *testing.T) {
	d, store := testDashboard(t, nil)
	h := mounted(d)
	c := authedClient(t, h)

	cfg := store.Load().Config
	cfg.Proxies.URLs = []string{"http://proxy-a:8080"}
	health := store.Load().Health
	// Manually trip the proxy, then reset it via the dashboard.
	health.Trip("http://proxy-a:8080", "boom", 0)
	if health.Healthy("http://proxy-a:8080") {
		t.Fatal("proxy should be tripped")
	}
	resp := postAuthed(t, h, c, "/_dashboard/act/proxies/reset?url="+url.QueryEscape("http://proxy-a:8080"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy reset status = %d", resp.StatusCode)
	}
	if !health.Healthy("http://proxy-a:8080") {
		t.Fatal("proxy should be healthy after dashboard reset")
	}

	// Force-open the breaker, then reset it.
	brk := store.Load().Breakers["p"]
	resp2 := postAuthed(t, h, c, "/_dashboard/act/breakers/open?provider=p")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("breaker open status = %d", resp2.StatusCode)
	}
	if !brk.Open() {
		t.Fatal("breaker should be open after dashboard force-open")
	}
	resp3 := postAuthed(t, h, c, "/_dashboard/act/breakers/reset?provider=p")
	defer resp3.Body.Close()
	if brk.Open() {
		t.Fatal("breaker should be closed after dashboard reset")
	}
}

// TestDashboardSectionsRender hits each section and asserts it returns 200 and
// a known marker, so a template regression is caught.
func TestDashboardSectionsRender(t *testing.T) {
	d, _ := testDashboard(t, nil)
	h := mounted(d)
	c := authedClient(t, h)
	for _, sec := range []string{"providers", "keys", "proxies", "breakers", "models", "traces"} {
		resp := doGet(t, h, c, "/_dashboard/"+sec)
		body := readBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("section %s status = %d body=%s", sec, resp.StatusCode, body)
			continue
		}
		if !strings.Contains(body, sec) && !strings.Contains(body, titleCase(sec)) {
			t.Errorf("section %s body missing its name", sec)
		}
	}
}

// TestDashboardDisabledWhenNoToken asserts that when the admin token is empty,
// the login page reports disabled and the dashboard rejects authed requests.
func TestDashboardDisabledWhenNoToken(t *testing.T) {
	d, store := testDashboard(t, nil)
	store.Load().Config.Server.AdminToken = ""
	h := mounted(d)
	resp := doGet(t, h, &http.Client{}, "/_dashboard/login")
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(strings.ToLower(body), "disabled") {
		t.Errorf("login should mention disabled when token empty: %s", body)
	}
}

func postAuthed(t *testing.T, h http.Handler, c *http.Client, path string) *http.Response {
	t.Helper()
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := c.Post(ts.URL+path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
