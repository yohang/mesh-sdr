// Package shell is the app shell module (UI epic): the admin-set look and
// feel, the navigation, the shell data every page renders with
// (render.ShellSource), the home and static pages, and the error pages of
// every path outside the API.
package shell

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yohang/mesh-sdr/internal/web"
	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// Deps are the shell module's dependencies.
type Deps struct {
	// Settings reads the effective settings (the settings store).
	Settings Values
	// AdminGate tells whether the visitor may open the admin area (admin
	// role, from an allowed network). Nil: the Admin section is never shown.
	AdminGate Gate
	// FilesGate tells whether the visitor may open the Files section (the
	// files module, from the listen policy). Nil: open to every visitor.
	FilesGate Gate
	// Bookmarks admits the visitors who manage the hub bookmarks (operators
	// and admins) and names their page: the receiver's Bookmarks tab then
	// links its add form, pre-filled from the tuning (BMK-001, BMK-003).
	// Optional.
	Bookmarks *BookmarksLink
	Logger    *slog.Logger
	// User returns the signed-in visitor of a request for the user menu
	// (nil: anonymous). Optional.
	User func(r *http.Request) *layout.User
	// Images tells whether a station image is set (Receiver page).
	// Optional.
	Images StationImages
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// Wired is the wired shell module.
type Wired struct {
	// Renderer renders pages in the shell; every module with HTML pages
	// takes it as a dependency.
	Renderer *render.Renderer
	// HTTP is the shell's router module.
	HTTP *Module
}

// New builds the shell module.
func New(d Deps) Wired {
	// Receiver, Map and Decodes are open to everyone (FEATURE_SPEC §10.2).
	files := d.FilesGate
	if files == nil {
		files = Everyone
	}

	now := d.Now
	if now == nil {
		now = time.Now
	}

	m := &Module{
		settings: NewSettings(d.Settings), static: web.Static(), markdown: newMarkdown(), admin: d.AdminGate,
		bookmarks: d.Bookmarks, user: d.User, images: d.Images, now: now,
		logger: d.Logger.With(slog.String("component", "shell.module")),
		gates: map[string]Gate{
			layout.SectionReceiver: Everyone, layout.SectionMap: Everyone, layout.SectionDecodes: Everyone,
			layout.SectionFiles: files, layout.SectionAdmin: d.AdminGate,
		},
	}

	var admin func(ctx context.Context) bool
	if d.AdminGate != nil {
		admin = d.AdminGate.Allows
	}

	m.render = render.New(m, admin, d.Logger.With(slog.String("component", "web.render")))

	return Wired{Renderer: m.render, HTTP: m}
}

// BookmarksLink is the Bookmarks › Manage page and who may open it.
type BookmarksLink struct {
	Gate Gate
	Path string
}
