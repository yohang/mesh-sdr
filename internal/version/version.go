// Package version holds the product version of the binary (ADR 0008):
// set at build time with -ldflags "-X github.com/yohang/mesh-sdr/internal/version.version=1.2.3",
// otherwise taken from the module build info, otherwise "dev". It also
// holds the licence and the source code location the UI shows (AGPL-3.0
// section 13): a build of modified sources sets its own with
// -ldflags "-X github.com/yohang/mesh-sdr/internal/version.sourceURL=https://…".
package version

import (
	"runtime/debug"
	"strings"
)

// version is set by the linker for release builds.
var version string

// sourceURL is where the source code of this build is published; the
// linker may override it.
var sourceURL = "https://github.com/yohang/mesh-sdr"

// License is the SPDX identifier of the product licence.
const License = "AGPL-3.0-or-later"

// SourceURL returns where the source code of this build is published.
func SourceURL() string { return sourceURL }

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
