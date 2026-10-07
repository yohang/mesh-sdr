package radio_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The node holds no DB connection (GRID-002): the device module layers
// never link the database packages (the module wiring reads config.Node
// only), and only internal/dsp/csdr uses cgo.
func TestNoDatabaseAndCgoBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{len .CgoFiles}}", "./domain/...", "./app/...", "./infra/...", "./http/...", "../dsp/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, out)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, cgo, _ := strings.Cut(line, " ")

		if strings.HasPrefix(pkg, "github.com/yohang/mesh-sdr/internal/db") || strings.Contains(pkg, "sqlite") {
			t.Errorf("the device module links %s", pkg)
		}

		if strings.HasPrefix(pkg, "github.com/yohang/mesh-sdr/") && cgo != "0" && pkg != "github.com/yohang/mesh-sdr/internal/dsp/csdr" {
			t.Errorf("%s uses cgo outside internal/dsp/csdr", pkg)
		}
	}
}
