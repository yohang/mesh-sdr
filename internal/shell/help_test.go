package shell_test

import (
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell"
)

func TestHelpLink(t *testing.T) {
	for _, ok := range []string{"", "https://docs.example.org/meshsdr", "http://lan.example:8080/help?x=1"} {
		h, err := shell.NewHelpLink(ok)
		if err != nil || h.String() != ok || h.IsZero() != (ok == "") {
			t.Errorf("NewHelpLink(%q) = %q, %v", ok, h, err)
		}
	}

	for _, bad := range []string{"/help", "javascript:alert(1)", "ftp://example.org", "https://", "https://user:pw@example.org", "docs.example.org"} {
		if _, err := shell.NewHelpLink(bad); !errors.Is(err, shell.ErrInvalidHelpLink) {
			t.Errorf("NewHelpLink(%q) error = %v", bad, err)
		}
	}
}

func TestShortcutSet(t *testing.T) {
	if s, err := shell.NewShortcutSet("default"); err != nil || !s.Enabled() || s.String() != "default" {
		t.Errorf("default = %v, %v", s, err)
	}

	if s, err := shell.NewShortcutSet("off"); err != nil || s.Enabled() || s.String() != "off" {
		t.Errorf("off = %v, %v", s, err)
	}

	if _, err := shell.NewShortcutSet("vim"); !errors.Is(err, shell.ErrInvalidShortcutSet) {
		t.Errorf("vim error = %v", err)
	}

	if !(shell.ShortcutSet{}).Enabled() {
		t.Error("zero value is not the default set")
	}
}
