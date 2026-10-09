package render

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/a-h/templ"

	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// Redirect sends the browser to path after an action: htmx follows
// HX-Redirect (a full page load), other clients a 303.
func Redirect(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", path)
		w.WriteHeader(http.StatusNoContent)

		return
	}

	http.Redirect(w, r, path, http.StatusSeeOther)
}

// NoIndex is the middleware of the pages that are per visitor (admin,
// account, sign-in): they are never indexed nor stored by caches.
func NoIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// ParseForm parses the form body of r, capped at limit bytes (0: the
// server cap only). On failure it writes the error page, 413 for a body
// over the cap and 400 otherwise, and returns false.
func (rd *Renderer) ParseForm(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if limit > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest

		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}

		rd.Error(w, r, status)

		return false
	}

	return true
}

// AdminPage writes a page of the admin area (FEATURE_SPEC §10.13): content
// in the admin layout, section (layout.AdminSections) marked current. The
// section list is the one the visitor may open: every section for an
// admin, the operator ones otherwise.
func (rd *Renderer) AdminPage(w http.ResponseWriter, r *http.Request, status int, title, section string, content, fragment templ.Component) {
	sections := layout.AdminSections
	if rd.admin != nil && !rd.admin(r.Context()) {
		sections = layout.OperatorAdminSections
	}

	rd.Page(w, r, status, layout.Page{Title: title, Section: layout.SectionAdmin},
		layout.AdminPage(section, sections, content), fragment)
}

// Sentence capitalises a message and ends it with a full stop.
func Sentence(s string) string {
	if s == "" {
		return s
	}

	c, n := utf8.DecodeRuneInString(s)
	s = string(unicode.ToUpper(c)) + s[n:]

	if !strings.HasSuffix(s, ".") {
		s += "."
	}

	return s
}

// ParseKHz reads a frequency typed in kHz (a list filter) and returns it
// in Hz.
func ParseKHz(s string) (int64, bool) {
	khz, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(khz) || khz < 0 || khz > 1e9 {
		return 0, false
	}

	return int64(math.Round(khz * 1000)), true
}

// maxPage bounds the page numbers of the lists.
const maxPage = 10_000

// PageOf reads the page number of a list (the page query parameter, from
// 1); a missing or invalid one is the first page.
func PageOf(q url.Values) int {
	n, err := strconv.Atoi(q.Get("page"))
	if err != nil || n < 1 || n > maxPage {
		return 1
	}

	return n
}
