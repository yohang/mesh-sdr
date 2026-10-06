// Package app holds the shell use cases.
package app

import (
	"context"
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/shell/domain"
)

// Defaults used when a setting cannot be read.
const (
	// DefaultSiteName is the site name when receiver.name is unavailable.
	DefaultSiteName = "MeshSDR"
	// DefaultPolicyURL is the usage policy link when
	// receiver.usage_policy_url is unavailable.
	DefaultPolicyURL = "/policy"
)

// Settings reads the shell's admin settings. Errors are infra failures or
// invalid stored values; callers fall back to the defaults.
type Settings interface {
	ThemeMode(ctx context.Context) (domain.ThemeMode, error)
	SiteName(ctx context.Context) (string, error)
	PolicyURL(ctx context.Context) (string, error)
	HelpLink(ctx context.Context) (domain.HelpLink, error)
	ShortcutSet(ctx context.Context) (domain.ShortcutSet, error)
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
	PolicyURL string
	// Help is the help link (UI-002); zero when unset.
	Help domain.HelpLink
	// Shortcuts is the keyboard shortcut set.
	Shortcuts domain.ShortcutSet
}

// View returns the current look and feel. A failing settings source never
// breaks a page: the default applies and the failure is logged.
func (l *LookAndFeel) View(ctx context.Context) View {
	v := View{SiteName: DefaultSiteName, ThemeMode: domain.DefaultThemeMode(), PolicyURL: DefaultPolicyURL}

	if mode, err := l.settings.ThemeMode(ctx); err != nil {
		l.logger.WarnContext(ctx, "theme mode unavailable, using the default",
			slog.String("default", v.ThemeMode.String()), slog.Any("error", err))
	} else {
		v.ThemeMode = mode
	}

	if name, err := l.settings.SiteName(ctx); err != nil {
		l.logger.WarnContext(ctx, "site name unavailable, using the default", slog.Any("error", err))
	} else {
		v.SiteName = name
	}

	if u, err := l.settings.PolicyURL(ctx); err != nil {
		l.logger.WarnContext(ctx, "usage policy link unavailable, using the default", slog.Any("error", err))
	} else {
		v.PolicyURL = u
	}

	if h, err := l.settings.HelpLink(ctx); err != nil {
		l.logger.WarnContext(ctx, "help link unavailable, hiding it", slog.Any("error", err))
	} else {
		v.Help = h
	}

	if set, err := l.settings.ShortcutSet(ctx); err != nil {
		l.logger.WarnContext(ctx, "shortcut set unavailable, using the default", slog.Any("error", err))
	} else {
		v.Shortcuts = set
	}

	return v
}
