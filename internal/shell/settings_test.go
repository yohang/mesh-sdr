package shell_test

import (
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// TestSettings: the getters read the effective settings and fill the
// defaults of empty values.
func TestSettings(t *testing.T) {
	set := shell.NewSettings(values{
		"ui.theme_mode": "dark", "receiver.name": " F4XYZ ", "receiver.usage_policy_url": "https://example.org/rules",
		"receiver.help_url": "https://docs.example.org", "ui.shortcut_set": "off", "receiver.usage_policy_text": "Be nice.",
		"audio_compression": "pcm",
	})

	if set.Theme() != layout.ThemeDark || set.SiteName() != "F4XYZ" || set.PolicyURL() != "https://example.org/rules" ||
		set.HelpURL() != "https://docs.example.org" || set.Shortcuts() || set.Policy() != "Be nice." || set.AudioCodec() != shell.CodecPCM {
		t.Errorf("settings = %+v", set)
	}

	empty := shell.NewSettings(values{"ui.theme_mode": "sepia", "receiver.name": " ", "receiver.usage_policy_url": ""})
	if empty.Theme() != layout.ThemeAuto || empty.SiteName() != shell.DefaultSiteName || empty.PolicyURL() != shell.DefaultPolicyURL ||
		empty.HelpURL() != "" || !empty.Shortcuts() || empty.AudioCodec() != shell.CodecADPCM {
		t.Error("defaults not applied")
	}

	if empty.Policy() != shell.DefaultPolicy() || !strings.Contains(shell.DefaultPolicy(), "## Acceptable use") {
		t.Error("default policy not embedded")
	}
}
