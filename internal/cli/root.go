// Package cli defines the meshsdr command line interface.
package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/log"
)

type app struct {
	cfg    config.Config
	logger *slog.Logger
}

// Execute runs the root command.
func Execute() error {
	return newRootCmd().Execute()
}

func newRootCmd() *cobra.Command {
	a := &app{}

	cmd := &cobra.Command{
		Use:           "meshsdr",
		Short:         "MeshSDR",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			logger, err := log.New(os.Stderr, cfg.LogLevel, cfg.LogFormat)
			if err != nil {
				return err
			}

			slog.SetDefault(logger)
			a.cfg, a.logger = cfg, logger

			return nil
		},
	}

	cmd.AddCommand(newServeCmd(a), newMigrateCmd(a))

	return cmd
}
