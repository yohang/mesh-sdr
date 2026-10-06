// Package infra holds the shell adapters.
package infra

import (
	"context"
	"fmt"
	"strings"

	"github.com/yohang/mesh-sdr/internal/shell/domain"
)

// Setting keys read by the shell (ADR 0010).
const (
	KeyThemeMode       = "ui.theme_mode"
	KeySiteName        = "receiver.name"
	KeyUsagePolicyText = "receiver.usage_policy_text"
	KeyUsagePolicyURL  = "receiver.usage_policy_url"
	KeyHelpURL         = "receiver.help_url"
	KeyShortcutSet     = "ui.shortcut_set"
)

// Values reads the current effective settings (the settings store).
type Values interface {
	String(key string) string
}

// StoreSettings reads the shell settings from the settings store: config
// (locked) > DB > default, read on every request so a saved change applies
// at once.
type StoreSettings struct {
	values Values
}

// NewStoreSettings returns the adapter.
func NewStoreSettings(values Values) *StoreSettings { return &StoreSettings{values: values} }

// ThemeMode returns ui.theme_mode.
func (s *StoreSettings) ThemeMode(context.Context) (domain.ThemeMode, error) {
	m, err := domain.NewThemeMode(s.values.String(KeyThemeMode))
	if err != nil {
		return domain.ThemeMode{}, fmt.Errorf("%s: %w", KeyThemeMode, err)
	}

	return m, nil
}

// SiteName returns receiver.name.
func (s *StoreSettings) SiteName(context.Context) (string, error) {
	name := strings.TrimSpace(s.values.String(KeySiteName))
	if name == "" {
		return "", fmt.Errorf("%s is empty", KeySiteName)
	}

	return name, nil
}

// PolicyURL returns receiver.usage_policy_url.
func (s *StoreSettings) PolicyURL(context.Context) (string, error) {
	u := s.values.String(KeyUsagePolicyURL)
	if u == "" {
		return "", fmt.Errorf("%s is empty", KeyUsagePolicyURL)
	}

	return u, nil
}

// HelpLink returns receiver.help_url (zero when unset).
func (s *StoreSettings) HelpLink(context.Context) (domain.HelpLink, error) {
	h, err := domain.NewHelpLink(strings.TrimSpace(s.values.String(KeyHelpURL)))
	if err != nil {
		return domain.HelpLink{}, fmt.Errorf("%s: %w", KeyHelpURL, err)
	}

	return h, nil
}

// ShortcutSet returns ui.shortcut_set.
func (s *StoreSettings) ShortcutSet(context.Context) (domain.ShortcutSet, error) {
	set, err := domain.NewShortcutSet(s.values.String(KeyShortcutSet))
	if err != nil {
		return domain.ShortcutSet{}, fmt.Errorf("%s: %w", KeyShortcutSet, err)
	}

	return set, nil
}

// UsagePolicy returns receiver.usage_policy_text; set is false when it is
// empty.
func (s *StoreSettings) UsagePolicy(context.Context) (domain.PolicyText, bool, error) {
	text := s.values.String(KeyUsagePolicyText)
	if strings.TrimSpace(text) == "" {
		return domain.PolicyText{}, false, nil
	}

	p, err := domain.NewPolicyText(text)
	if err != nil {
		return domain.PolicyText{}, false, fmt.Errorf("%s: %w", KeyUsagePolicyText, err)
	}

	return p, true, nil
}
