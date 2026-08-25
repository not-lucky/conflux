package dashboard

import (
	"embed"
	"io/fs"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// templateFiles returns the embedded HTML templates as an fs.FS rooted at
// the templates directory.
func templateFiles() fs.FS {
	sub, err := fs.Sub(templateFS, "templates")
	if err != nil {
		panic(err)
	}
	return sub
}

// assetFiles returns the embedded static assets (css, js, vendored htmx) as
// an fs.FS rooted at the assets directory.
func assetFiles() fs.FS {
	sub, err := fs.Sub(assetFS, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}
