package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/wire"
)

func (a *app) newAllCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "all",
		Short: "Start a hub and its local node in one process",
		Long: "Start the hub and a local node (default id \"local\" on 127.0.0.1:8074), from hub.toml and\n" +
			"node.toml in the same config directory. On first start the hub CA (tls.ca_cert, tls.ca_key)\n" +
			"and the local node certificate (tls.cert, tls.key) are created when absent; the local node is\n" +
			"enrolled in-process, with no token. Remote nodes can be enrolled as with `meshsdr hub`.\n" +
			"Like the hub, it refuses to start while database migrations are pending.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runAll(cmd.Context()) },
	}

	check := &cobra.Command{
		Use:   "config",
		Short: "Inspect the all configuration",
	}
	check.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Load and validate hub.toml and node.toml with the all-role defaults, without starting",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.allConfigCheck() },
	})

	cmd.AddCommand(check)

	return cmd
}

func (a *app) allConfigCheck() error {
	opts := a.configOptions()
	opts.DeferSecrets = true

	_, hubMeta, _, nodeMeta, err := config.LoadAll(opts)
	if err != nil {
		return err
	}

	files := append(slices.Clone(hubMeta.Files), nodeMeta.Files...)

	if a.json {
		return a.printJSON(map[string]any{
			"valid": true, "role": "all", "files": files,
			"origins":  map[string]any{"hub": origins(hubMeta), "node": origins(nodeMeta)},
			"warnings": append(slices.Clone(hubMeta.Warnings), nodeMeta.Warnings...),
		})
	}

	a.print("all configuration is valid (files: %s)", strings.Join(files, ", "))
	a.print("hub.toml:")
	a.printOrigins(hubMeta)
	a.print("node.toml:")
	a.printOrigins(nodeMeta)

	return nil
}

func (a *app) runAll(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts := a.configOptions()
	dir := opts.ResolvedDir()

	// First start: the paths of the files to create, before the secrets
	// they hold are read.
	pre := opts
	pre.DeferSecrets = true

	preHub, _, _, _, err := config.LoadAll(pre)
	if err != nil {
		return err
	}

	keyPath, ok := preHub.TLS.CAKey.FilePath(dir)
	if !ok {
		return fmt.Errorf("the all role needs tls.ca_key as a { file = \"…\" } reference")
	}

	created, err := wire.EnsureCA(preHub.TLS.CACert, keyPath, time.Now())
	if err != nil {
		return err
	}

	hubCfg, hubMeta, nodeCfg, nodeMeta, err := config.LoadAll(opts)
	if err != nil {
		return err
	}

	logger, err := a.newLogger(hubCfg.Log)
	if err != nil {
		return err
	}

	logConfig(ctx, logger, config.RoleHub, hubMeta)
	logConfig(ctx, logger, config.RoleNode, nodeMeta)

	if created {
		logger.InfoContext(ctx, "hub CA created", slog.String("ca_cert", hubCfg.TLS.CACert))
	}

	adapter, err := wire.OpenDB(ctx, hubCfg.DB, logger)
	if err != nil {
		return err
	}

	defer func() { _ = adapter.Close() }()

	if err := adapter.Migrator().Check(ctx); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}

	logger.InfoContext(ctx, "all starting", slog.String("url", hubCfg.Hub.URL), slog.String("node_id", nodeCfg.Node.ID),
		slog.String("node_listen", nodeCfg.Node.Listen))

	p, err := wire.All(ctx, hubCfg, hubMeta.Origins, nodeCfg, logger, adapter)
	if err != nil {
		return err
	}

	if u := p.Hub().SetupURL(); u != "" {
		a.printSetupURL(u)
	}

	if err := p.Run(ctx); err != nil {
		return err
	}

	logger.InfoContext(ctx, "all stopped")

	return nil
}
