package shell_test

import (
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell"
)

func TestSections(t *testing.T) {
	var got []string
	for _, s := range shell.Sections() {
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

	s, err := shell.NewSection("files")
	if err != nil || s != shell.SectionFiles {
		t.Errorf("NewSection(files) = %v, %v", s, err)
	}

	if _, err := shell.NewSection("settings"); !errors.Is(err, shell.ErrInvalidSection) {
		t.Errorf("NewSection(settings) error = %v", err)
	}

	if !(shell.Section{}).IsZero() || shell.SectionMap.IsZero() {
		t.Error("IsZero")
	}
}
