package shell_test

import (
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell"
)

func TestNewThemeMode(t *testing.T) {
	tests := []struct {
		in                  string
		light, dark, isAuto bool
	}{
		{"light", true, false, false},
		{"dark", false, true, false},
		{"auto", false, false, true},
	}

	for _, tt := range tests {
		m, err := shell.NewThemeMode(tt.in)
		if err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}

		if m.String() != tt.in || m.IsLight() != tt.light || m.IsDark() != tt.dark || m.IsAuto() != tt.isAuto {
			t.Errorf("%s: got %s light=%v dark=%v auto=%v", tt.in, m, m.IsLight(), m.IsDark(), m.IsAuto())
		}
	}

	for _, bad := range []string{"", "Light", "sepia", "auto "} {
		if _, err := shell.NewThemeMode(bad); !errors.Is(err, shell.ErrInvalidThemeMode) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}

	if d := shell.DefaultThemeMode(); !d.IsAuto() || d.String() != "auto" {
		t.Errorf("default = %s", d)
	}

	if m, _ := shell.NewThemeMode("dark"); m != shell.MustThemeMode("dark") {
		t.Error("theme modes are not compared by value")
	}
}
