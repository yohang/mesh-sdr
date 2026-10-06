// Package version holds the product version of the binary (ADR 0008):
// set at build time with -ldflags "-X github.com/yohang/mesh-sdr/internal/version.version=1.2.3",
// otherwise taken from the module build info, otherwise "dev".
package version

import (
	"runtime/debug"
	"strings"
)

// version is set by the linker for release builds.
var version string

// String returns the product version, without a leading "v".
func String() string {
	if version != "" {
		return strings.TrimPrefix(version, "v")
	}

	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}

	return "dev"
}
