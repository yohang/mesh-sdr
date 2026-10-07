package shell_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell"
)

func TestNewPolicyText(t *testing.T) {
	p, err := shell.NewPolicyText("  # Rules\n\nBe nice.\n")
	if err != nil {
		t.Fatal(err)
	}

	if p.Markdown() != "# Rules\n\nBe nice." {
		t.Errorf("markdown = %q", p.Markdown())
	}

	for _, bad := range []string{"", " \n\t", strings.Repeat("é", shell.MaxPolicyTextLength+1), "\xff"} {
		if _, err := shell.NewPolicyText(bad); !errors.Is(err, shell.ErrInvalidPolicyText) {
			t.Errorf("%.20q: err = %v", bad, err)
		}
	}

	if _, err := shell.NewPolicyText(strings.Repeat("é", shell.MaxPolicyTextLength)); err != nil {
		t.Errorf("max length: %v", err)
	}
}
