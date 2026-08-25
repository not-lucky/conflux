// Package dashboard implements the Conflux management console: an HTMX-driven
// HTML UI embedded in the gateway at /_dashboard. It renders live gateway
// state (providers, key pools, proxies, breakers, models, traces) and exposes
// write actions (key retire/reset, proxy trip/reset, breaker reset/open,
// config reload) by operating on the swappable runtime snapshot and the
// stable observers.
//
// dashboard imports the concrete leaf packages (config, model, keypool, proxy,
// breaker, metrics, trace, redact, version) plus runtime for the live
// snapshot, exactly as the server package does. It is wired into the HTTP mux
// by the composition root (cmd/conflux) and is gated by server.admin_token.
package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/not-lucky/conflux/internal/metrics"
	"github.com/not-lucky/conflux/internal/redact"
	"github.com/not-lucky/conflux/internal/runtime"
	"github.com/not-lucky/conflux/internal/trace"
)

// adminCookie is the cookie name holding the admin token for the session.
const adminCookie = "conflux_admin"

// tracesPerPage is the maximum trace directories listed per page.
const tracesPerPage = 50

// Dashboard is the HTTP handler for /_dashboard. It reads live state through
// the runtime.Store and the stable metrics/tracer, and applies write actions
// to the live snapshot's pools/breakers/health. Reload is delegated to the
// injected reload function so dashboard stays decoupled from the composition
// root.
type Dashboard struct {
	live    *runtime.Store
	metrics *metrics.Registry
	tracer  *trace.Tracer
	reload  func() error
	assets  http.Handler
	r       *renderer
}

// New builds the Dashboard. reload is the function that re-reads config.yaml
// and rebuilds the runtime (supplied by the composition root); it may be nil,
// in which case the reload action returns 503.
func New(live *runtime.Store, mreg *metrics.Registry, tr *trace.Tracer, reload func() error) *Dashboard {
	d := &Dashboard{
		live: live, metrics: mreg, tracer: tr, reload: reload,
		r: mustRenderer(),
	}
	d.assets = http.FileServer(http.FS(assetFiles()))
	return d
}

// mustRenderer parses templates at construction; a parse error is a programmer
// bug (bad embedded template) and should fail loudly.
func mustRenderer() *renderer {
	r, err := newRenderer()
	if err != nil {
		panic(fmt.Sprintf("dashboard: parse templates: %v", err))
	}
	return r
}

// Routes returns the HTTP handler, mounted by the server at /_dashboard/.
// The login and asset routes are public; every other route is gated by the
// admin token via authGate. Auth failure redirects browser GETs to the login
// page and returns 401 JSON for HTMX/POST.
//
// To avoid Go ServeMux's trailing-slash redirect collisions (a registered
// "/keys/" would redirect a bare "/keys" section link), almost everything is
// routed through one catch-all "/" dispatcher that parses the action or
// section from the remaining path. Only assets, login, logout, and the
// overview partial have explicit patterns.
func (d *Dashboard) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/assets/", d.handleAsset)
	mux.HandleFunc("/login", d.handleLogin)
	mux.HandleFunc("/logout", d.handleLogout)
	mux.HandleFunc("/partials/overview", d.authGate(d.handleOverviewPartial))
	mux.HandleFunc("/", d.authGate(d.handleDispatch))
	return mux
}

// handleDispatch parses the remaining path (after /_dashboard has been
// stripped) and routes to the section page, a write action, the trace list, or
// a trace detail view.
func (d *Dashboard) handleDispatch(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	if path == "" {
		path = "overview"
	}
	// Write actions live under the "act/" prefix.
	if strings.HasPrefix(path, "act/") {
		d.handleAction(w, r, strings.TrimPrefix(path, "act/"))
		return
	}
	// Trace detail is "trace" with a ?dir= query.
	if path == "trace" {
		d.handleTraceDetail(w, r)
		return
	}
	// Trace list is "traces".
	if path == "traces" {
		d.handleTraces(w, r)
		return
	}
	// Otherwise it is a section page.
	if !validSection(path) {
		http.NotFound(w, r)
		return
	}
	payload := d.buildSection(path)
	out, err := d.r.renderPage(path, "Conflux · "+titleCase(path), payload)
	if err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(out)
}

// ---------------- auth ----------------

// adminToken returns the current admin token from the live config, or "".
func (d *Dashboard) adminToken() string {
	l := d.live.Load()
	if l == nil || l.Config == nil {
		return ""
	}
	return l.Config.Server.AdminToken
}

// requireAuth reports whether the request is authorized. When the admin token
// is empty the dashboard is disabled and this returns false. Otherwise the
// request's cookie must match the current token.
func (d *Dashboard) requireAuth(r *http.Request) bool {
	tok := d.adminToken()
	if tok == "" {
		return false
	}
	c, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	return subtleEqual(c.Value, tok)
}

// subtleEqual does a constant-time-ish string comparison so a cookie mismatch
// does not leak length info. (Not strictly necessary for an ops token, but
// cheap and correct.)
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// authGate wraps a handler with admin-token auth. On failure it redirects a
// browser GET to /_dashboard/login and returns 401 JSON for HTMX/POST.
func (d *Dashboard) authGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.requireAuth(r) {
			if r.Method == http.MethodGet && wantsHTML(r) {
				http.Redirect(w, r, "/_dashboard/login", http.StatusSeeOther)
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "unauthorized: dashboard requires a valid admin token",
			})
			return
		}
		h(w, r)
	}
}

func wantsHTML(r *http.Request) bool {
	acc := r.Header.Get("Accept")
	return strings.Contains(acc, "text/html") || acc == "" || !strings.Contains(acc, "application/json")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------- handlers ----------------

// handleAsset serves embedded static assets (css, js, vendored htmx) with
// long-lived cache headers. It is the one unauthenticated route so the login
// page can render.
func (d *Dashboard) handleAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/assets/")
	d.assets.ServeHTTP(w, r)
}

// handleLogin shows the login form (GET) and validates + sets the cookie
// (POST). When the admin token is empty it shows a "dashboard disabled" page.
func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		tok := d.adminToken()
		msg := ""
		if tok == "" {
			msg = "Dashboard is disabled. Set server.admin_token in config.yaml to enable it."
		}
		out, _ := d.r.renderLogin(msg)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(out)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok := d.adminToken()
	if tok == "" {
		http.Error(w, "dashboard disabled", http.StatusServiceUnavailable)
		return
	}
	_ = r.ParseForm()
	got := strings.TrimSpace(r.FormValue("token"))
	if !subtleEqual(got, tok) {
		out, _ := d.r.renderLogin("Invalid admin token.")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(out)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: tok, Path: "/_dashboard",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 0,
	})
	http.Redirect(w, r, "/_dashboard/", http.StatusSeeOther)
}

// handleLogout clears the session cookie.
func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Path: "/_dashboard", MaxAge: -1})
	http.Redirect(w, r, "/_dashboard/login", http.StatusSeeOther)
}

// handleTraces renders the trace list. A direct browser navigation renders the
// full page; an HTMX request (HX-Request header) renders just the partial for
// pagination swaps. The `before` query param is the pagination cursor.
func (d *Dashboard) handleTraces(w http.ResponseWriter, r *http.Request) {
	before := r.URL.Query().Get("before")
	data := d.buildTraces(before)
	if r.Header.Get("HX-Request") == "true" {
		out, err := d.r.renderSection("traces", "Traces", data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(out)
		return
	}
	out, err := d.r.renderPage("traces", "Conflux · Traces", data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(out)
}

// handleTraceDetail renders a single trace directory. It accepts `dir` (the
// trace dir base name) and optional `file` (which trace file to display).
func (d *Dashboard) handleTraceDetail(w http.ResponseWriter, r *http.Request) {
	dir := strings.TrimSpace(r.URL.Query().Get("dir"))
	file := r.URL.Query().Get("file")
	if dir == "" {
		// The path itself may be the dir name: /_dashboard/traces/<dirname>
		dir = strings.TrimPrefix(r.URL.Path, "/traces/")
		dir = strings.Trim(dir, "/")
	}
	if dir == "" {
		http.NotFound(w, r)
		return
	}
	data := d.buildTraceDetail(dir, file)
	if r.Header.Get("HX-Request") == "true" {
		out, err := d.r.renderSection("trace", "Trace", data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(out)
		return
	}
	out, err := d.r.renderPage("trace", "Conflux · Trace", data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(out)
}

// handleAction dispatches a write action of the form "<kind>/<verb>" where
// kind is keys, proxies, breakers, or reload. On success each handler returns
// the refreshed section partial so the UI swaps in place.
func (d *Dashboard) handleAction(w http.ResponseWriter, r *http.Request, action string) {
	parts := strings.SplitN(action, "/", 2)
	if len(parts) != 2 {
		if action == "reload" {
			d.handleReload(w, r)
			return
		}
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	kind, verb := parts[0], parts[1]
	switch kind {
	case "keys":
		d.handleKeyAction(w, r, verb)
	case "proxies":
		d.handleProxyAction(w, r, verb)
	case "breakers":
		d.handleBreakerAction(w, r, verb)
	default:
		http.Error(w, "unknown action kind", http.StatusBadRequest)
	}
}

// handleOverviewPartial renders just the overview content for the 5s HTMX
// poll, swapped into #overview-region.
func (d *Dashboard) handleOverviewPartial(w http.ResponseWriter, r *http.Request) {
	out, err := d.r.renderSection("overview", "Overview", d.buildSection("overview"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(out)
}

// validSection reports whether name maps to a rendered content template.
func validSection(name string) bool {
	switch name {
	case "overview", "providers", "keys", "proxies", "breakers", "models", "traces", "trace":
		return true
	}
	return false
}

func titleCase(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------------- write actions ----------------

// handleReload re-reads config.yaml and rebuilds the runtime via the injected
// reload function. On success it returns the fresh overview partial so the UI
// reflects the new state immediately.
func (d *Dashboard) handleReload(w http.ResponseWriter, r *http.Request) {
	if d.reload == nil {
		http.Error(w, "reload not configured", http.StatusServiceUnavailable)
		return
	}
	if err := d.reload(); err != nil {
		// Surface the config error verbatim so the operator sees the typo.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("reload failed: " + err.Error()))
		return
	}
	out, err := d.r.renderSection("overview", "Overview", d.buildSection("overview"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(out)
}

// handleKeyAction applies the per-key reset/retire action, then returns the
// refreshed keys partial. verb is "reset" or "retire".
func (d *Dashboard) handleKeyAction(w http.ResponseWriter, r *http.Request, verb string) {
	l := d.live.Load()
	provider := query(r, "provider")
	pool, ok := l.Pools[provider]
	if !ok {
		http.Error(w, "unknown provider", http.StatusBadRequest)
		return
	}
	key, err := strconv.Atoi(query(r, "key"))
	if err != nil {
		http.Error(w, "bad key number", http.StatusBadRequest)
		return
	}
	switch verb {
	case "reset":
		pool.Reset(key)
	case "retire":
		pool.MarkExhausted(key)
	default:
		http.Error(w, "unknown key action", http.StatusBadRequest)
		return
	}
	d.pushKeyGauges(l, provider)
	d.writeSection(w, "keys")
}

// handleProxyAction applies the per-proxy trip/reset action. verb is "reset"
// or "trip".
func (d *Dashboard) handleProxyAction(w http.ResponseWriter, r *http.Request, verb string) {
	l := d.live.Load()
	raw := query(r, "url")
	u, err := url.QueryUnescape(raw)
	if err != nil || u == "" {
		http.Error(w, "bad proxy url", http.StatusBadRequest)
		return
	}
	switch verb {
	case "reset":
		l.Health.Reset(u)
	case "trip":
		l.Health.Trip(u, "manually tripped via dashboard", 0)
	default:
		http.Error(w, "unknown proxy action", http.StatusBadRequest)
		return
	}
	d.pushProxyGauges(l)
	d.writeSection(w, "proxies")
}

// handleBreakerAction applies the per-provider breaker reset/open action.
// verb is "reset" or "open".
func (d *Dashboard) handleBreakerAction(w http.ResponseWriter, r *http.Request, verb string) {
	l := d.live.Load()
	prov := query(r, "provider")
	brk, ok := l.Breakers[prov]
	if !ok {
		http.Error(w, "unknown provider", http.StatusBadRequest)
		return
	}
	switch verb {
	case "reset":
		brk.Reset()
	case "open":
		brk.ForceOpen(0)
	default:
		http.Error(w, "unknown breaker action", http.StatusBadRequest)
		return
	}
	d.writeSection(w, "breakers")
}

// pushKeyGauges refreshes the key-state gauges for one provider after a
// management mutation, so /metrics stays correct.
func (d *Dashboard) pushKeyGauges(l *runtime.Live, provider string) {
	if pool, ok := l.Pools[provider]; ok {
		c := pool.Counts()
		d.metrics.SetKeysGauge(provider, int64(c.Active), int64(c.Standby), int64(c.Exhausted), int64(c.Retired))
	}
}

// pushProxyGauges refreshes all proxy-health gauges after a management
// mutation.
func (d *Dashboard) pushProxyGauges(l *runtime.Live) {
	for _, e := range l.Health.Snapshot(l.Config.ProxyURLs()) {
		d.metrics.SetProxyHealthy(redact.URL(e.URL), e.Healthy)
	}
}

// writeSection renders a section partial for an HTMX swap.
func (d *Dashboard) writeSection(w http.ResponseWriter, section string) {
	out, err := d.r.renderSection(section, titleCase(section), d.buildSection(section))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(out)
}

func query(r *http.Request, key string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	_ = r.ParseForm()
	return r.FormValue(key)
}
