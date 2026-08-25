package dashboard

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"

	"github.com/not-lucky/conflux/internal/version"
)

// navItem is one sidebar entry. Path is the section name used in the URL and
// to mark the active link; Icon names an inline SVG.
type navItem struct {
	Path  string
	Icon  string
	Label string
}

// nav is the fixed sidebar navigation.
var nav = []navItem{
	{"", "overview", "Overview"},
	{"providers", "provider", "Providers"},
	{"keys", "keys", "Keys"},
	{"proxies", "proxy", "Proxies"},
	{"breakers", "breaker", "Breakers"},
	{"models", "model", "Models"},
	{"traces", "trace", "Traces"},
}

// page is the top-level view model passed to the layout. The section
// content is pre-rendered into Body (as safe HTML) so the layout can inject
// it without a dynamic template name (which Go templates do not support);
// the section payload is still carried in Payload for any layout-level
// inspection if needed.
type page struct {
	Title   string
	Active  string
	Version string
	Nav     []navItem
	Payload any
	Body    template.HTML
}

// tmpl is the parsed template tree. It is parsed once at construction (see
// newRenderer) and safe for concurrent execution because html/template
// Template.Execute is concurrency-safe.
type renderer struct {
	t *template.Template
}

// newRenderer parses all embedded templates into one tree and registers the
// shared func map. A parse failure is fatal: the dashboard cannot render
// without its templates.
func newRenderer() (*renderer, error) {
	funcs := template.FuncMap{
		"svg":          svgIcon,
		"pct":          pct,
		"keyStatePill": keyStatePill,
	}
	t := template.New("dashboard").Funcs(funcs)
	files, err := fs.ReadDir(templateFiles(), ".")
	if err != nil {
		return nil, fmt.Errorf("read templates: %w", err)
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		raw, err := fs.ReadFile(templateFiles(), f.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Name(), err)
		}
		if _, err := t.New(f.Name()).Parse(string(raw)); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f.Name(), err)
		}
	}
	return &renderer{t: t}, nil
}

// renderPage renders the full layout for the given section. The section
// partial is rendered first (as in renderSection) and injected into the
// layout's Body so the layout does not need a dynamic template name (Go
// templates require a literal name in the {{template}} action).
func (r *renderer) renderPage(active, title string, payload any) ([]byte, error) {
	body, err := r.renderSection(active, title, payload)
	if err != nil {
		return nil, err
	}
	p := page{
		Title:   title,
		Active:  active,
		Version: version.Version,
		Nav:     nav,
		Payload: payload,
		Body:    template.HTML(body),
	}
	var buf bytes.Buffer
	if err := r.t.ExecuteTemplate(&buf, "layout", p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderSection renders only the "content-<active>" partial (no layout),
// suitable for an HTMX swap that replaces a region. The content template is
// executed directly against the section payload, which is what those
// templates expect (.Payload is unwrapped by the layout's dispatch).
func (r *renderer) renderSection(active, title string, payload any) ([]byte, error) {
	var buf bytes.Buffer
	if err := r.t.ExecuteTemplate(&buf, "content-"+active, payload); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderLogin renders the standalone login page.
func (r *renderer) renderLogin(errMsg string) ([]byte, error) {
	var buf bytes.Buffer
	if err := r.t.ExecuteTemplate(&buf, "login", map[string]any{"Error": errMsg}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pct returns the integer percentage of n/total, clamped to [0,100], used to
// size the key-state stacked bars. Returns 0 when total is 0.
func pct(n, total int) int {
	if total <= 0 {
		return 0
	}
	p := n * 100 / total
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// keyStatePill renders the HTML pill for a key state string. The state is one
// of "active", "standby", "exhausted", "retired".
func keyStatePill(state string) template.HTML {
	cls := state
	label := state
	switch state {
	case "active":
		cls = "active"
	case "standby":
		cls = "standby"
	case "exhausted":
		cls = "exhausted"
	case "retired":
		cls = "retired"
	default:
		cls = "muted"
	}
	return template.HTML(fmt.Sprintf(`<span class="pill %s">%s</span>`, cls, label))
}
