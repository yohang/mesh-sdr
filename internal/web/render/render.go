// Package render writes HTML responses in the app shell. Every module renders
// its pages through a Renderer, so the full page vs fragment decision (ADR
// 0003, ADR 0007) and the response headers live in one place.
package render

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"

	"github.com/a-h/templ"

	"github.com/yohang/mesh-sdr/internal/http/redact"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// Vary lists the request headers the full page vs fragment decision reads.
const Vary = "HX-Request, HX-Boosted, HX-Request-Type, HX-History-Restore-Request"

// ShellSource builds the shell data of a request (site name, theme, links).
// It never fails: on a degraded source it logs and falls back to defaults.
type ShellSource interface {
	Shell(r *http.Request) layout.Shell
}

// Renderer renders pages and error pages in the app shell.
type Renderer struct {
	shell  ShellSource
	admin  func(ctx context.Context) bool
	logger *slog.Logger
}

// New returns a Renderer. admin tells whether the visitor of a request is
// an admin, for the admin section list (AdminPage); nil shows every
// section.
func New(shell ShellSource, admin func(ctx context.Context) bool, logger *slog.Logger) *Renderer {
	return &Renderer{shell: shell, admin: admin, logger: logger}
}

// WantsFragment reports whether r asks for a fragment rather than the full
// page. htmx sends HX-Request on every request, but boosted navigation and
// history restores need the full page (htmx keeps its #main), as does any
// request htmx marks as full (it selects part of the response).
func WantsFragment(r *http.Request) bool {
	h := r.Header

	return h.Get("HX-Request") == "true" &&
		h.Get("HX-Boosted") == "" &&
		h.Get("HX-History-Restore-Request") == "" &&
		h.Get("HX-Request-Type") != "full"
}

// Page writes a page with status. content is the page body (inside #main).
// fragment, when not nil, is written alone instead of the full page when the
// request asks for a fragment (WantsFragment); a page with no fragment
// representation passes nil and always gets the full page.
func (rd *Renderer) Page(w http.ResponseWriter, r *http.Request, status int, page layout.Page, content, fragment templ.Component) {
	c := templ.Component(layout.Document(rd.shell.Shell(r), page, content))
	if fragment != nil && WantsFragment(r) {
		c = fragment
	}

	rd.write(w, r, status, c)
}

// Error writes the error page for status (4xx, 5xx): the full shell page, or
// only the error content when the request asks for a fragment (htmx swaps
// error responses into the request's target, which must not receive a
// second shell).
func (rd *Renderer) Error(w http.ResponseWriter, r *http.Request, status int) {
	title := http.StatusText(status)
	content := layout.Error(status, title, errorMessage(status))

	if WantsFragment(r) {
		rd.write(w, r, status, content)

		return
	}

	rd.write(w, r, status, layout.Document(rd.shell.Shell(r), layout.Page{Title: title}, content))
}

// NotFound writes the 404 page.
func (rd *Renderer) NotFound(w http.ResponseWriter, r *http.Request) {
	rd.Error(w, r, http.StatusNotFound)
}

// MethodNotAllowed writes the 405 page.
func (rd *Renderer) MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	rd.Error(w, r, http.StatusMethodNotAllowed)
}

func errorMessage(status int) string {
	switch status {
	case http.StatusForbidden:
		return "You do not have access to this page."
	case http.StatusNotFound:
		return "This page does not exist."
	case http.StatusMethodNotAllowed:
		return "This page does not accept this request."
	default:
		if status >= http.StatusInternalServerError {
			return "Something went wrong on the server. Try again later."
		}

		return "The request could not be processed."
	}
}

// write buffers the rendering, so a template error still produces a clean
// 500 response, then sends it with the HTML response headers. Responses are
// per request (session, role, theme), so they are never stored by caches.
func (rd *Renderer) write(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer

	h := w.Header()
	h.Add("Vary", Vary)
	h.Set("Cache-Control", "no-store")

	if err := c.Render(r.Context(), &buf); err != nil {
		rd.logger.ErrorContext(r.Context(), "render page",
			slog.String("path", redact.Path(r.URL.Path)), slog.Int("status", status), slog.Any("error", err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if _, err := buf.WriteTo(w); err != nil {
		rd.logger.DebugContext(r.Context(), "write page", slog.String("path", redact.Path(r.URL.Path)), slog.Any("error", err))
	}
}
