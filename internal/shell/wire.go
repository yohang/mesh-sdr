// Package shell is the app shell module (UI epic): the admin-set look and
// feel, the navigation, the shell data every page renders with
// (render.ShellSource), the home and static pages, and the error pages of
// every path outside the API.
package shell

import (
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

// Wire builds the shell module.
func Wire(d Deps) Wired {
	component := func(name string) *slog.Logger { return d.Logger.With(slog.String("component", name)) }

	settings := NewStoreSettings(d.Settings)
	lookAndFeel := NewLookAndFeel(settings, component("shell.app.look_and_feel"))
	policy := NewPolicy(settings, component("shell.app.policy"))
	// Receiver, Map, Decodes and Files are open to everyone until their
	// modules bring their own access policies (FEATURE_SPEC §10.2).
	nav := NewNavigation(map[Section]Gate{
		SectionReceiver: Everyone,
		SectionMap:      Everyone,
		SectionDecodes:  Everyone,
		SectionFiles:    Everyone,
		SectionAdmin:    d.AdminGate,
	})
	now := d.Now
	if now == nil {
		now = time.Now
	}

	source := NewShellSource(lookAndFeel, nav, d.User, now)
	rd := render.New(source, component("web.render"))

	station := NewStation(settings, d.Images)

	return Wired{
		Renderer: rd,
		HTTP:     NewModule(rd, source, policy, station, web.Static(), component("shell.http")),
	}
}
