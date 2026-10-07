package domain_test

import (
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell/domain"
)

func TestSections(t *testing.T) {
	var got []string
	for _, s := range domain.Sections() {
		got = append(got, s.ID()+" "+s.Label()+" "+s.Path())
	}

	want := []string{"receiver Receiver /", "map Map /map", "decodes Decodes /decodes", "files Files /files", "admin Admin /admin"}
	if len(got) != len(want) {
		t.Fatalf("sections = %v", got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section %d = %q, want %q", i, got[i], want[i])
		}
	}

	s, err := domain.NewSection("files")
	if err != nil || s != domain.SectionFiles {
		t.Errorf("NewSection(files) = %v, %v", s, err)
	}

	if _, err := domain.NewSection("settings"); !errors.Is(err, domain.ErrInvalidSection) {
		t.Errorf("NewSection(settings) error = %v", err)
	}

	if !(domain.Section{}).IsZero() || domain.SectionMap.IsZero() {
		t.Error("IsZero")
	}
}
