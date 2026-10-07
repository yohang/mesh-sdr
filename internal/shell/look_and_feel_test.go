package shell_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell"
)

type settings struct {
	mode shell.ThemeMode
	err  error
}

func (s settings) ThemeMode(context.Context) (shell.ThemeMode, error) { return s.mode, s.err }
func (s settings) SiteName(context.Context) (string, error)           { return "F4XYZ", s.err }
func (s settings) PolicyURL(context.Context) (string, error) {
	return "https://example.org/rules", s.err
}

func (s settings) HelpLink(context.Context) (shell.HelpLink, error) {
	h, _ := shell.NewHelpLink("https://docs.example.org")

	return h, s.err
}

func (s settings) ShortcutSet(context.Context) (shell.ShortcutSet, error) {
	set, _ := shell.NewShortcutSet("off")

	return set, s.err
}

func TestLookAndFeel(t *testing.T) {
	var logs bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&logs, nil))

	v := shell.NewLookAndFeel(settings{mode: shell.MustThemeMode("dark")}, logger).View(context.Background())
	if !v.ThemeMode.IsDark() || v.SiteName != "F4XYZ" || v.PolicyURL != "https://example.org/rules" ||
		v.Help.String() != "https://docs.example.org" || v.Shortcuts.Enabled() {
		t.Errorf("view = %+v", v)
	}

	if logs.Len() != 0 {
		t.Errorf("unexpected logs: %s", logs.String())
	}

	v = shell.NewLookAndFeel(settings{err: errors.New("db down")}, logger).View(context.Background())
	if !v.ThemeMode.IsAuto() || v.SiteName != shell.DefaultSiteName || v.PolicyURL != shell.DefaultPolicyURL ||
		!v.Help.IsZero() || !v.Shortcuts.Enabled() {
		t.Errorf("fallback = %s, want auto", v.ThemeMode)
	}

	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "db down") {
		t.Errorf("fallback not logged at Warn: %s", logs.String())
	}
}
