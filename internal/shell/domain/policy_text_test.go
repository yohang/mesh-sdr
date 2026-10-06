package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell/domain"
)

func TestNewPolicyText(t *testing.T) {
	p, err := domain.NewPolicyText("  # Rules\n\nBe nice.\n")
	if err != nil {
		t.Fatal(err)
	}

	if p.Markdown() != "# Rules\n\nBe nice." {
		t.Errorf("markdown = %q", p.Markdown())
	}

	for _, bad := range []string{"", " \n\t", strings.Repeat("é", domain.MaxPolicyTextLength+1), "\xff"} {
		if _, err := domain.NewPolicyText(bad); !errors.Is(err, domain.ErrInvalidPolicyText) {
			t.Errorf("%.20q: err = %v", bad, err)
		}
	}

	if _, err := domain.NewPolicyText(strings.Repeat("é", domain.MaxPolicyTextLength)); err != nil {
		t.Errorf("max length: %v", err)
	}
}
