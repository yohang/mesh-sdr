package shell

import (
	"context"
	"fmt"
	"strings"
)

// Setting keys read by the shell (ADR 0010).
const (
	KeyThemeMode       = "ui.theme_mode"
	KeySiteName        = "receiver.name"
	KeyUsagePolicyText = "receiver.usage_policy_text"
	KeyUsagePolicyURL  = "receiver.usage_policy_url"
	KeyHelpURL         = "receiver.help_url"
	KeyShortcutSet     = "ui.shortcut_set"
	KeyLocation        = "receiver.location"
	KeyPhotoTitle      = "receiver.photo_title"
	KeyPhotoDesc       = "receiver.photo_desc"
	KeyAudioCompress   = "audio_compression"
	KeyRecorderEnabled = "ui.recorder_enabled"
)

// Values reads the current effective settings (the settings store).
type Values interface {
	String(key string) string
	Bool(key string) bool
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
func (s *StoreSettings) ThemeMode(context.Context) (ThemeMode, error) {
	m, err := NewThemeMode(s.values.String(KeyThemeMode))
	if err != nil {
		return ThemeMode{}, fmt.Errorf("%s: %w", KeyThemeMode, err)
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
func (s *StoreSettings) HelpLink(context.Context) (HelpLink, error) {
	h, err := NewHelpLink(strings.TrimSpace(s.values.String(KeyHelpURL)))
	if err != nil {
		return HelpLink{}, fmt.Errorf("%s: %w", KeyHelpURL, err)
	}

	return h, nil
}

// ShortcutSet returns ui.shortcut_set.
func (s *StoreSettings) ShortcutSet(context.Context) (ShortcutSet, error) {
	set, err := NewShortcutSet(s.values.String(KeyShortcutSet))
	if err != nil {
		return ShortcutSet{}, fmt.Errorf("%s: %w", KeyShortcutSet, err)
	}

	return set, nil
}

// UsagePolicy returns receiver.usage_policy_text; set is false when it is
// empty.
func (s *StoreSettings) UsagePolicy(context.Context) (PolicyText, bool, error) {
	text := s.values.String(KeyUsagePolicyText)
	if strings.TrimSpace(text) == "" {
		return PolicyText{}, false, nil
	}

	p, err := NewPolicyText(text)
	if err != nil {
		return PolicyText{}, false, fmt.Errorf("%s: %w", KeyUsagePolicyText, err)
	}

	return p, true, nil
}

// Location returns receiver.location.
func (s *StoreSettings) Location(context.Context) string {
	return strings.TrimSpace(s.values.String(KeyLocation))
}

// PhotoTitle returns receiver.photo_title.
func (s *StoreSettings) PhotoTitle(context.Context) string {
	return strings.TrimSpace(s.values.String(KeyPhotoTitle))
}

// PhotoDesc returns receiver.photo_desc (Markdown).
func (s *StoreSettings) PhotoDesc(context.Context) string { return s.values.String(KeyPhotoDesc) }

// AudioCompression returns audio_compression (adpcm or pcm).
func (s *StoreSettings) AudioCompression(context.Context) string {
	return s.values.String(KeyAudioCompress)
}

// RecorderEnabled returns ui.recorder_enabled.
func (s *StoreSettings) RecorderEnabled(context.Context) bool {
	return s.values.Bool(KeyRecorderEnabled)
}
