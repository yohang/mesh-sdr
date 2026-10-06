// Package app holds the shell use cases.
package app

import (
	"context"
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/shell/domain"
)

// SiteName is the product name shown in the shell until the receiver name
// setting exists (settings.receiver.name).
const SiteName = "MeshSDR"

// Settings reads the shell's admin settings. Errors are infra failures or
// invalid stored values; callers fall back to the defaults.
type Settings interface {
	ThemeMode(ctx context.Context) (domain.ThemeMode, error)
}

// LookAndFeel returns the admin-set look and feel of the shell.
type LookAndFeel struct {
	settings Settings
	logger   *slog.Logger
}

// NewLookAndFeel returns the use case.
func NewLookAndFeel(settings Settings, logger *slog.Logger) *LookAndFeel {
	return &LookAndFeel{settings: settings, logger: logger}
}

// View is the look and feel of one request.
type View struct {
	SiteName  string
	ThemeMode domain.ThemeMode
}

// View returns the current look and feel. A failing settings source never
// breaks a page: the default applies and the failure is logged.
func (l *LookAndFeel) View(ctx context.Context) View {
	mode, err := l.settings.ThemeMode(ctx)
	if err != nil {
		l.logger.WarnContext(ctx, "theme mode unavailable, using the default",
			slog.String("default", domain.DefaultThemeMode().String()), slog.Any("error", err))

		mode = domain.DefaultThemeMode()
	}

	return View{SiteName: SiteName, ThemeMode: mode}
}
