package shell

import (
	_ "embed"
	"strings"

	"github.com/yohang/mesh-sdr/internal/web/layout"
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

// Defaults of the settings that may be empty.
const (
	// DefaultSiteName is the site name when receiver.name is empty.
	DefaultSiteName = "MeshSDR"
	// DefaultPolicyURL is the usage policy link when
	// receiver.usage_policy_url is empty.
	DefaultPolicyURL = "/policy"
)

//go:embed default_policy.md
var defaultPolicy string

// DefaultPolicy returns the built-in usage policy (Markdown), shown until
// the admin sets one (receiver.usage_policy_text).
func DefaultPolicy() string { return defaultPolicy }

// Values reads the current effective settings (the settings store).
type Values interface {
	String(key string) string
	Bool(key string) bool
}

// Settings reads the shell's settings: config (locked) > DB > default,
// read on every call so a saved change applies at once. The settings
// catalogue validates every value (enums, URLs, lengths); the getters only
// fill the defaults of empty values.
type Settings struct{ values Values }

// NewSettings returns the settings read from values.
func NewSettings(values Values) Settings { return Settings{values: values} }

func (s Settings) text(key string) string { return strings.TrimSpace(s.values.String(key)) }

// Theme returns ui.theme_mode (UI-001); auto unless light or dark.
func (s Settings) Theme() layout.Theme {
	switch t := layout.Theme(s.values.String(KeyThemeMode)); t {
	case layout.ThemeLight, layout.ThemeDark:
		return t
	default:
		return layout.ThemeAuto
	}
}

// SiteName returns receiver.name.
func (s Settings) SiteName() string {
	if name := s.text(KeySiteName); name != "" {
		return name
	}

	return DefaultSiteName
}

// PolicyURL returns receiver.usage_policy_url.
func (s Settings) PolicyURL() string {
	if u := s.text(KeyUsagePolicyURL); u != "" {
		return u
	}

	return DefaultPolicyURL
}

// HelpURL returns receiver.help_url (UI-002); "" hides the help links.
func (s Settings) HelpURL() string { return s.text(KeyHelpURL) }

// Shortcuts reports whether single-key shortcuts are on (ui.shortcut_set
// is not off).
func (s Settings) Shortcuts() bool { return s.values.String(KeyShortcutSet) != "off" }

// Policy returns receiver.usage_policy_text (UI-003, Markdown), or the
// default policy when it is empty.
func (s Settings) Policy() string {
	if p := s.text(KeyUsagePolicyText); p != "" {
		return p
	}

	return DefaultPolicy()
}

// Location returns receiver.location.
func (s Settings) Location() string { return s.text(KeyLocation) }

// PhotoTitle returns receiver.photo_title.
func (s Settings) PhotoTitle() string { return s.text(KeyPhotoTitle) }

// PhotoDesc returns receiver.photo_desc (Markdown).
func (s Settings) PhotoDesc() string { return s.values.String(KeyPhotoDesc) }

// AudioCodec returns the codec the receiver asks the nodes for
// (audio_compression): CodecPCM for pcm, else CodecADPCM.
func (s Settings) AudioCodec() string {
	if s.values.String(KeyAudioCompress) == "pcm" {
		return CodecPCM
	}

	return CodecADPCM
}

// RecorderEnabled returns ui.recorder_enabled.
func (s Settings) RecorderEnabled() bool { return s.values.Bool(KeyRecorderEnabled) }
