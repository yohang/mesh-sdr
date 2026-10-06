package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/yohang/mesh-sdr/internal/web/templates"
)

// SPIKE: hardcoded theme mode until the ui_theme setting exists (UI-001).
const prototypeTheme = templates.ThemeAuto

// navItems is the full navigation, in display order.
//
// SPIKE: every section is shown. UI-006 gates them by role (Admin for admins only,
// Files and Decodes per their own policies) once sessions carry a role.
var navItems = []templates.NavItem{
	{Section: templates.SectionReceiver, Label: "Receiver", Href: "/"},
	{Section: templates.SectionMap, Label: "Map", Href: "/map"},
	{Section: templates.SectionDecodes, Label: "Decodes", Href: "/decodes"},
	{Section: templates.SectionFiles, Label: "Files", Href: "/files"},
	{Section: templates.SectionAdmin, Label: "Admin", Href: "/admin"},
}

type pages struct {
	logger *slog.Logger
}

// shell builds the layout data for the current request.
func (p *pages) shell(r *http.Request, active templates.Section, title string) templates.Shell {
	s, _ := sessionFrom(r.Context())

	return templates.Shell{
		Title:     title,
		Active:    active,
		Theme:     prototypeTheme,
		CSRFToken: s.csrfToken(),
		Nav:       navItems,
	}
}

// render writes a full page. Boosted navigation also gets the full page and keeps
// only #main (hx-select), so one URL always returns one representation (no Vary).
func (p *pages) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if err := c.Render(r.Context(), w); err != nil {
		p.logger.ErrorContext(r.Context(), "render page", slog.String("path", r.URL.Path), slog.Any("error", err))
	}
}

func (p *pages) receiver(w http.ResponseWriter, r *http.Request) {
	// SPIKE: fake devices; the real list comes from the grid (UI-021). The second name
	// checks that untrusted text survives JSONScript + island rendering as text.
	data := templates.ReceiverBootstrap{Devices: []templates.DeviceSummary{
		{ID: "rtl-0", Name: "RTL-SDR v4", Node: "hub", CenterFrequencyHz: 145_500_000},
		{ID: "airspy-0", Name: `</script><img src=x onerror=alert(1)>`, Node: "node-2", CenterFrequencyHz: 433_920_000},
	}}

	p.render(w, r, http.StatusOK, templates.ReceiverPage(p.shell(r, templates.SectionReceiver, "Receiver"), data))
}

func (p *pages) mapPage(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, templates.MapPage(p.shell(r, templates.SectionMap, "Map")))
}

func (p *pages) decodes(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, templates.DecodesPage(p.shell(r, templates.SectionDecodes, "Decodes")))
}

func (p *pages) files(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, templates.FilesPage(p.shell(r, templates.SectionFiles, "Files")))
}

func (p *pages) admin(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, templates.AdminPage(p.shell(r, templates.SectionAdmin, "Admin")))
}

func (p *pages) notFound(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusNotFound, templates.NotFoundPage(p.shell(r, "", "Page not found")))
}

// adminDemo is the SPIKE CSRF demo endpoint: JSON body in, HTML fragment out.
func (p *pages) adminDemo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Note string `json:"note"`
	}

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	note := strings.TrimSpace(in.Note)
	if note == "" || len(note) > 80 {
		http.Error(w, "note must be 1 to 80 characters", http.StatusUnprocessableEntity)
		return
	}

	p.render(w, r, http.StatusOK, templates.AdminDemoResult(note, time.Now().UTC()))
}
