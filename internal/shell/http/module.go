// Package http serves the app shell: the shell data every page renders with
// (render.ShellSource), the home and static pages, and the error pages of
// every path outside the API.
package http

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/shell/app"
	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

//go:generate go tool templ generate

// ShellSource builds the per-request shell data from the look and feel.
type ShellSource struct {
	lookAndFeel *app.LookAndFeel
}

// NewShellSource returns a ShellSource.
func NewShellSource(lookAndFeel *app.LookAndFeel) *ShellSource {
	return &ShellSource{lookAndFeel: lookAndFeel}
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

	return layout.Shell{
		SiteName: v.SiteName,
		Theme:    theme,
		// The link target becomes settings.receiver.usage_policy_url (ADM-035).
		FooterLinks: []layout.Link{{Label: "Usage policy", Href: "/policy"}},
	}
}

// Module is the shell's router module (internal/http.Module).
type Module struct {
	render   *render.Renderer
	policy   *app.Policy
	markdown *markdown
	logger   *slog.Logger
}

// NewModule returns the shell router module.
func NewModule(rd *render.Renderer, policy *app.Policy, logger *slog.Logger) *Module {
	return &Module{render: rd, policy: policy, markdown: newMarkdown(), logger: logger}
}

// Middlewares implements internal/http.Module: the shell has none.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module. The shell owns the HTML 404 and
// 405 pages of the whole router; the API keeps its problem+json errors.
func (m *Module) Routes(r chi.Router) {
	r.NotFound(m.render.NotFound)
	r.MethodNotAllowed(m.render.MethodNotAllowed)

	r.Get("/", m.home)
	r.Get("/robots.txt", robots)
	r.Get("/policy", m.policyPage)
}

// home is a placeholder home page until the Receiver section (UI-006) takes
// over "/".
func (m *Module) home(w http.ResponseWriter, r *http.Request) {
	m.render.Page(w, r, http.StatusOK, layout.Page{}, homePage(app.SiteName), nil)
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
