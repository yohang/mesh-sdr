package radio_test

import (
	"os/exec"
	"strings"
	"testing"
)

// goList returns "<import path> <number of cgo files>" for the packages.
func goList(t *testing.T, patterns ...string) [][2]string {
	t.Helper()

	args := append([]string{"list", "-deps", "-f", "{{.ImportPath}} {{len .CgoFiles}}"}, patterns...)

	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, out)
	}

	var pkgs [][2]string

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, cgo, _ := strings.Cut(line, " ")
		pkgs = append(pkgs, [2]string{pkg, cgo})
	}

	return pkgs
}

// The node holds no DB connection (GRID-002): the device module layers
// never link the database packages (the module wiring reads config.Node
// only).
func TestNoDatabase(t *testing.T) {
	for _, p := range goList(t, "./domain/...", "./app/...", "./infra/...", "./http/...", "../dsp/...") {
		if strings.HasPrefix(p[0], "github.com/yohang/mesh-sdr/internal/db") || strings.Contains(p[0], "sqlite") {
			t.Errorf("the device module links %s", p[0])
		}
	}
}

// Only internal/dsp/csdr uses cgo in the whole module (ADR 0014, ADR 0019).
func TestCgoBoundary(t *testing.T) {
	for _, p := range goList(t, "github.com/yohang/mesh-sdr/...") {
		if strings.HasPrefix(p[0], "github.com/yohang/mesh-sdr/") && p[1] != "0" && p[0] != "github.com/yohang/mesh-sdr/internal/dsp/csdr" {
			t.Errorf("%s uses cgo outside internal/dsp/csdr", p[0])
		}
	}
}
