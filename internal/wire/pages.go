package wire

import (
	"net/http"

	"github.com/a-h/templ"

	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// pages adapts the shell renderer to the identity module's Pages: module
// pages are rendered in the app shell, full page unless htmx asks for a
// fragment (ADR 0007).
type pages struct{ rd *render.Renderer }

// Page implements identityhttp.Pages.
func (p pages) Page(w http.ResponseWriter, r *http.Request, status int, title string, content, fragment templ.Component) {
	p.rd.Page(w, r, status, layout.Page{Title: title}, content, fragment)
}

// Error implements identityhttp.Pages.
func (p pages) Error(w http.ResponseWriter, r *http.Request, status int) {
	p.rd.Error(w, r, status)
}
