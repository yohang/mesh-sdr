// Package shell is the app shell module (UI epic): look and feel, layout data,
// static pages and the HTML error pages.
package shell

import (
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/shell/app"
	shellhttp "github.com/yohang/mesh-sdr/internal/shell/http"
	"github.com/yohang/mesh-sdr/internal/shell/infra"
	"github.com/yohang/mesh-sdr/internal/web"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// Deps are the shell module's dependencies.
type Deps struct {
	// Settings reads the effective settings (the settings store).
	Settings infra.Values
	Logger   *slog.Logger
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
	source := shellhttp.NewShellSource(lookAndFeel)
	rd := render.New(source, component("web.render"))

	return Module{
		Renderer: rd,
		HTTP:     shellhttp.NewModule(rd, source, policy, web.Static(), component("shell.http")),
	}
}
