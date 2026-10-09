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

	cfg, logger, err := a.loadNode(ctx)
	if err != nil {
		return err
	}

	p, err := wire.Node(cfg, logger, time.Now())
	if err != nil {
		return err
	}

	if wire.NodeEnrolled(cfg) {
		logger.InfoContext(ctx, "node starting: enrolled, serving the mTLS node API",
			slog.String("node_id", cfg.Node.ID), slog.String("listen", cfg.Node.Listen))
	} else {
		logger.InfoContext(ctx, "node starting: not enrolled, serving the pre-enrollment API only (run `meshsdr node enroll`)",
			slog.String("node_id", cfg.Node.ID), slog.String("listen", cfg.Node.Listen))
	}

	if err := p.Run(ctx); err != nil {
		return err
	}

	logger.InfoContext(ctx, "node stopped")

	return nil
}

// loadNode loads the node config and builds the logger.
func (a *app) loadNode(ctx context.Context) (config.Node, *slog.Logger, error) {
	cfg, meta, err := config.LoadNode(a.configOptions())
	if err != nil {
		return config.Node{}, nil, err
	}

	logger, err := a.newLogger(cfg.Log)
	if err != nil {
		return config.Node{}, nil, err
	}

	logConfig(ctx, logger, config.RoleNode, meta)

	return cfg, logger, nil
}
