// Package web holds templates and embedded static assets.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var staticFS embed.FS

// Static returns the static assets filesystem, rooted at the static directory.
func Static() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}

	return sub
}
