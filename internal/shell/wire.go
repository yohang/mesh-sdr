// Package shell is the app shell module (UI epic): look and feel, layout data,
// static pages and the HTML error pages.
package shell

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/yohang/mesh-sdr/internal/shell/app"
	"github.com/yohang/mesh-sdr/internal/shell/domain"
	shellhttp "github.com/yohang/mesh-sdr/internal/shell/http"
	"github.com/yohang/mesh-sdr/internal/shell/infra"
	"github.com/yohang/mesh-sdr/internal/web"
	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// Deps are the shell module's dependencies.
type Deps struct {
	// Settings reads the effective settings (the settings store).
	Settings infra.Values
	// AdminGate tells whether the visitor may open the admin area (admin
	// role, from an allowed network). Nil: the Admin section is never shown.
	AdminGate app.Gate
	Logger    *slog.Logger
	// User returns the signed-in user of a request for the top bar (nil:
	// anonymous). Optional.
	User func(r *http.Request) *layout.User
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// Module is the wired shell module.
type Module struct {
	// Renderer renders pages in the shell; every module with HTML pages
	// takes it as a dependency.
	Renderer *render.Renderer
	// HTTP is the shell's router module.
	HTTP *shellhttp.Module
}

// Wire builds the shell module.
func Wire(d Deps) Module {
	component := func(name string) *slog.Logger { return d.Logger.With(slog.String("component", name)) }

	settings := infra.NewStoreSettings(d.Settings)
	lookAndFeel := app.NewLookAndFeel(settings, component("shell.app.look_and_feel"))
	policy := app.NewPolicy(settings, component("shell.app.policy"))
	// Receiver, Map, Decodes and Files are open to everyone until their
	// modules bring their own access policies (FEATURE_SPEC §10.2).
	nav := app.NewNavigation(map[domain.Section]app.Gate{
		domain.SectionReceiver: app.Everyone,
		domain.SectionMap:      app.Everyone,
		domain.SectionDecodes:  app.Everyone,
		domain.SectionFiles:    app.Everyone,
		domain.SectionAdmin:    d.AdminGate,
	})
	now := d.Now
	if now == nil {
		now = time.Now
	}

	source := shellhttp.NewShellSource(lookAndFeel, nav, d.User, now)
	rd := render.New(source, component("web.render"))

	return Module{
		Renderer: rd,
		HTTP:     shellhttp.NewModule(rd, source, policy, web.Static(), component("shell.http")),
	}
}
