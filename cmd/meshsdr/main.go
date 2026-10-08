// Command meshsdr is the single MeshSDR binary: meshsdr hub, meshsdr node.
package main

import (
	"context"
	"os"

	"github.com/yohang/mesh-sdr/internal/cli"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

func main() {
	// The node re-executes itself as the exec helper of the tools it
	// supervises (ADR 0017): it must run before anything else, cobra
	// included, and never returns in that mode.
	process.MaybeRunExecHelper()

	os.Exit(cli.Execute(context.Background(), os.Args[1:]))
}
