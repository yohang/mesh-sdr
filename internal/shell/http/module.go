// Package http serves the app shell: the shell data every page renders with
// (render.ShellSource), the home and static pages, and the error pages of
// every path outside the API.
package http

import (
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/shell/app"
	"github.com/yohang/mesh-sdr/internal/shell/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

//go:generate go tool templ generate

// ShellSource builds the per-request shell data from the look and feel and
// the sections the visitor may open.
type ShellSource struct {
	lookAndFeel *app.LookAndFeel
	nav         *app.Navigation
	user        func(r *http.Request) *layout.User
	now         func() time.Time
}

// NewShellSource returns a ShellSource. user returns the signed-in visitor
// of a request for the user menu (nil: anonymous); it may be nil. now is
// the clock of the top bar.
func NewShellSource(lookAndFeel *app.LookAndFeel, nav *app.Navigation, user func(r *http.Request) *layout.User,
	now func() time.Time,
) *ShellSource {
	return &ShellSource{lookAndFeel: lookAndFeel, nav: nav, user: user, now: now}
}

// Shell implements render.ShellSource.
func (s *ShellSource) Shell(r *http.Request) layout.Shell {
	v := s.lookAndFeel.View(r.Context())

	theme := layout.ThemeAuto

	switch {
	case v.ThemeMode.IsLight():
		theme = layout.ThemeLight
	case v.ThemeMode.IsDark():
		theme = layout.ThemeDark
	}

	var nav []layout.Link
	for _, sec := range s.nav.Sections(r.Context()) {
		nav = append(nav, layout.Link{Label: sec.Label(), Href: sec.Path(), Section: sec.ID()})
	}

	var user *layout.User
	if s.user != nil {
		user = s.user(r)
	}

	return layout.Shell{
		User:        user,
		SiteName:    v.SiteName,
		Theme:       theme,
		FooterLinks: []layout.Link{{Label: "Usage policy", Href: v.PolicyURL}},
		Nav:         nav,
		Now:         s.now(),
	}
}

// Module is the shell's router module (internal/http.Module).
type Module struct {
	render   *render.Renderer
	shell    render.ShellSource
	policy   *app.Policy
	static   fs.FS
	markdown *markdown
	logger   *slog.Logger
}

// NewModule returns the shell router module. static is the embedded static
// assets filesystem (web.Static).
func NewModule(rd *render.Renderer, shell render.ShellSource, policy *app.Policy, static fs.FS, logger *slog.Logger) *Module {
	return &Module{render: rd, shell: shell, policy: policy, static: static, markdown: newMarkdown(), logger: logger}
}

// Middlewares implements internal/http.Module: the shell has none.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module. The shell owns the HTML 404 and
// 405 pages of the whole router; the API keeps its problem+json errors.
func (m *Module) Routes(r chi.Router) {
	r.NotFound(m.render.NotFound)
	r.MethodNotAllowed(m.methodNotAllowed)

	// Read-only pages answer GET and HEAD (net/http drops HEAD bodies).
	get := func(pattern string, h http.HandlerFunc) {
		r.Get(pattern, h)
		r.Head(pattern, h)
	}

	get("/", m.receiver)
	get(domain.SectionMap.Path(), m.placeholder(domain.SectionMap, "The live map is not available yet."))
	get(domain.SectionDecodes.Path(), m.placeholder(domain.SectionDecodes, "Decoded messages are not available yet."))
	get(domain.SectionFiles.Path(), m.placeholder(domain.SectionFiles, "Received files are not available yet."))
	get("/robots.txt", robots)
	get("/policy", m.policyPage)
	get("/manifest.webmanifest", m.manifest)
	get("/favicon.ico", favicon(m.static))
}

// allowCandidates are the methods probed for the Allow header of a 405.
var allowCandidates = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// methodNotAllowed is the router's 405 page. chi gives a custom 405 handler
// no Allow header, so the methods the path accepts are probed on the router.
func (m *Module) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.Routes != nil {
		var allowed []string

		for _, method := range allowCandidates {
			if rctx.Routes.Match(chi.NewRouteContext(), method, r.URL.Path) {
				allowed = append(allowed, method)
			}
		}

		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
		}
	}

	m.render.MethodNotAllowed(w, r)
}

// receiver is the Receiver section's entry page. Until the receiver exists
// (M1), it shows the station and says so.
func (m *Module) receiver(w http.ResponseWriter, r *http.Request) {
	page := layout.Page{Section: domain.SectionReceiver.ID()}
	m.render.Page(w, r, http.StatusOK, page, receiverPage(m.shell.Shell(r).SiteName), nil)
}

// placeholder serves the entry page of a section whose module does not
// exist yet (Map, Decodes, Files): its heading and a short notice. The
// section's module takes the route over when it lands.
func (m *Module) placeholder(sec domain.Section, notice string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := layout.Page{Title: sec.Label(), Section: sec.ID()}
		m.render.Page(w, r, http.StatusOK, page, placeholderPage(sec.Label(), notice), nil)
	}
}

// policyPage serves the usage policy (UI-003), public.
func (m *Module) policyPage(w http.ResponseWriter, r *http.Request) {
	html, err := m.markdown.HTML(m.policy.Text(r.Context()).Markdown())
	if err != nil {
		m.logger.ErrorContext(r.Context(), "render usage policy", slog.Any("error", err))
		m.render.Error(w, r, http.StatusInternalServerError)

		return
	}

	m.render.Page(w, r, http.StatusOK, layout.Page{Title: "Usage policy"}, policyPage(html), nil)
}
