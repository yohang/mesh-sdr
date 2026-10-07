// Package web holds templates and embedded static assets.
package web

import (
	"embed"
	"io/fs"
)

// ReceiverWorkletPath is the URL path of the receiver audio worklet. A
// worklet request carries no CSP nonce: the CSP lists this exact file (ADR
// 0015 decision 5), so it must stay a static embedded asset, served without
// redirect.
const ReceiverWorkletPath = "/static/js/receiver/rx-worklet.js"

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
