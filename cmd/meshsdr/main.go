package main

import (
	"log/slog"
	"os"

	"github.com/yohang/mesh-sdr/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		slog.Error("command failed", slog.Any("error", err))
		os.Exit(1)
	}
}
