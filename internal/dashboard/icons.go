package dashboard

import "html/template"

// svgIcon returns inline SVG markup for a named icon, so the dashboard ships
// with zero icon HTTP requests. Sizes are fixed at 16px (sidebar/nav) except
// "logo" which is sized by CSS via its parent. All icons use currentColor so
// they inherit text color and the active-link accent.
func svgIcon(name string) template.HTML {
	switch name {
	case "logo":
		return tmpl(`<svg class="logo" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 3 3 6v6c0 4.5 3.4 8.7 8 9 4.6-.3 8-4.5 8-9V6l-5-3"/><path d="M8 3v8M16 3v8M3 9h5M16 9h5"/></svg>`)
	case "overview":
		return ic(`<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/>`)
	case "provider":
		return ic(`<path d="M2 6a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2z"/><path d="M2 10h20"/><path d="M6 6h.01M9 6h.01"/>`)
	case "keys":
		return ic(`<circle cx="8" cy="15" r="4"/><path d="m10.8 12.2 8.2-8.2a2 2 0 0 1 3 0"/><path d="m18 2 2 2M21 5l2 2"/>`)
	case "proxy":
		return ic(`<path d="M12 4 2 9l10 5 10-5-10-5z"/><path d="M2 9v6l10 5 10-5V9"/><path d="M12 14v7"/>`)
	case "breaker":
		return ic(`<path d="M13 2 3 14h7l-1 8 10-12h-7z"/>`)
	case "model":
		return ic(`<path d="M4 5h16M4 12h16M4 19h10"/>`)
	case "trace":
		return ic(`<path d="M3 12h4l3-8 4 16 3-8h4"/>`)
	case "rule":
		return ic(`<path d="M3 3h18v18H3z"/><path d="M7 8h6M7 12h10M7 16h4"/>`)
	case "chart":
		return ic(`<path d="M3 3v18h18"/><path d="M7 14l3-4 3 3 4-6"/>`)
	case "info":
		return ic(`<circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/>`)
	case "file":
		return ic(`<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/>`)
	default:
		return template.HTML("")
	}
}

// ic wraps a raw icon path in a standard 16x16 svg with stroke=currentColor.
func ic(path string) template.HTML {
	return tmpl(`<svg class="ic" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + path + `</svg>`)
}

// tmpl is a tiny helper that casts a string to safe HTML, keeping the icon
// helpers readable.
func tmpl(s string) template.HTML { return template.HTML(s) }
