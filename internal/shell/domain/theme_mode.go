// Package domain holds the shell bounded context: the admin-set look and feel
// and the static pages of the app shell.
package domain

import (
	"strconv"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ErrInvalidThemeMode is returned for a theme mode other than light, dark or auto.
var ErrInvalidThemeMode = shared.NewError(shared.KindInvalid, "invalid_theme_mode",
	"theme mode must be light, dark or auto")

// Theme mode values.
const (
	themeLight = "light"
	themeDark  = "dark"
	themeAuto  = "auto"
)

// ThemeMode is the admin-chosen theme mode (UI-001): light, dark, or auto,
// which follows the visitor's prefers-color-scheme. There is no user override.
type ThemeMode struct {
	value string
}

// NewThemeMode validates s and returns it as a ThemeMode.
func NewThemeMode(s string) (ThemeMode, error) {
	switch s {
	case themeLight, themeDark, themeAuto:
		return ThemeMode{value: s}, nil
	default:
		return ThemeMode{}, ErrInvalidThemeMode.WithDetail("invalid theme mode " + strconv.Quote(s) + ": must be light, dark or auto")
	}
}

// MustThemeMode is NewThemeMode that panics on error. Tests and constants only.
func MustThemeMode(s string) ThemeMode {
	m, err := NewThemeMode(s)
	if err != nil {
		panic(err)
	}

	return m
}

// DefaultThemeMode returns the built-in default, auto.
func DefaultThemeMode() ThemeMode { return ThemeMode{value: themeAuto} }

// IsLight reports whether the light theme is forced.
func (m ThemeMode) IsLight() bool { return m.value == themeLight }

// IsDark reports whether the dark theme is forced.
func (m ThemeMode) IsDark() bool { return m.value == themeDark }

// IsAuto reports whether the theme follows prefers-color-scheme. The zero
// value is auto.
func (m ThemeMode) IsAuto() bool { return m.value == themeAuto || m.value == "" }

// String returns light, dark or auto.
func (m ThemeMode) String() string {
	if m.value == "" {
		return themeAuto
	}

	return m.value
}
