package cli

import (
	"context"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/wire"
)

func (a *app) newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Start a node (SDR devices and DSP)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.runNode(cmd.Context()) },
	}

	cmd.AddCommand(a.newConfigCmd(config.RoleNode), a.newEnrollCmd())

	return cmd
}

func (a *app) runNode(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, meta, err := config.LoadNode(a.configOptions())
	if err != nil {
		return err
	}

	logger, err := a.newLogger(cfg.Log)
	if err != nil {
		return err
	}

	logConfig(ctx, logger, config.RoleNode, meta)

	p, err := wire.Node(cfg, logger, time.Now())
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "node starting: not enrolled, serving the pre-enrollment API only",
		slog.String("node_id", cfg.Node.ID), slog.String("listen", cfg.Node.Listen))

	if err := p.Run(ctx); err != nil {
		return err
	}

	logger.InfoContext(ctx, "node stopped")

	return nil
}
