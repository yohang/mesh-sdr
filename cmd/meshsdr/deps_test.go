package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestBinaryDependencies checks the build list of meshsdr: the OpenAPI
// tooling (kin-openapi, and apitest which uses it) is build- and
// test-time only and must never be linked into the binary (ADR 0013).
func TestBinaryDependencies(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found")
	}

	out, err := exec.Command(gobin, "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}

	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "github.com/getkin/kin-openapi") || pkg == "github.com/yohang/mesh-sdr/internal/http/api/apitest" {
			t.Errorf("meshsdr links %s", pkg)
		}
	}
}
