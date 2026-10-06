// Command meshsdr is the single MeshSDR binary: meshsdr hub, meshsdr node.
package main

import (
	"context"
	"os"

	"github.com/yohang/mesh-sdr/internal/cli"
)

func main() {
	os.Exit(cli.Execute(context.Background(), os.Args[1:]))
}
