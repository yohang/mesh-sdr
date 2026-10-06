package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/wire"
)

func (a *app) newHubCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hub",
		Short: "Start the hub (web UI, REST API, database)",
		Long: "Start the hub. It refuses to start while database migrations are pending:\n" +
			"apply them with `meshsdr hub migrate`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runHub(cmd.Context()) },
	}

	cmd.AddCommand(a.newMigrateCmd(), a.newConfigCmd(config.RoleHub))

	return cmd
}

// loadHub loads the hub config and builds the logger.
func (a *app) loadHub(ctx context.Context) (config.Hub, *slog.Logger, error) {
	cfg, meta, err := config.LoadHub(a.configOptions())
	if err != nil {
		return config.Hub{}, nil, err
	}

	logger, err := a.newLogger(cfg.Log)
	if err != nil {
		return config.Hub{}, nil, err
	}

	logConfig(ctx, logger, config.RoleHub, meta)

	return cfg, logger, nil
}

func (a *app) runHub(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, logger, err := a.loadHub(ctx)
	if err != nil {
		return err
	}

	adapter, err := wire.OpenDB(ctx, cfg.DB, logger)
	if err != nil {
		return err
	}

	defer func() { _ = adapter.Close() }()

	if err := adapter.Migrator().Check(ctx); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}

	logger.InfoContext(ctx, "hub starting", slog.String("listen", cfg.Hub.Listen), slog.String("url", cfg.Hub.URL))

	if err := wire.Hub(cfg, logger, adapter).Run(ctx); err != nil {
		return err
	}

	logger.InfoContext(ctx, "hub stopped")

	return nil
}

func (a *app) newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations (same as `migrate up`)",
		Long: "Manage the database schema. Migrations are forward-only and checksummed;\n" +
			"run them while the hub is stopped.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.migrateUp(cmd.Context()) },
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "up",
			Short: "Apply all pending migrations",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return a.migrateUp(cmd.Context()) },
		},
		&cobra.Command{
			Use:   "down",
			Short: "Roll back the latest migration (development only)",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return a.migrateDown(cmd.Context()) },
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show the migrations and whether they are applied",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return a.migrateStatus(cmd.Context()) },
		},
	)

	return cmd
}

// withMigrator opens the hub database and calls fn with its migrator.
func (a *app) withMigrator(ctx context.Context, fn func(db.Migrator, *slog.Logger) error) error {
	cfg, logger, err := a.loadHub(ctx)
	if err != nil {
		return err
	}

	adapter, err := wire.OpenDB(ctx, cfg.DB, logger)
	if err != nil {
		return err
	}

	defer func() { _ = adapter.Close() }()

	return fn(adapter.Migrator(), logger)
}

type migrationJSON struct {
	Version    int64     `json:"version"`
	Name       string    `json:"name"`
	Applied    bool      `json:"applied"`
	AppliedAt  time.Time `json:"applied_at,omitzero"`
	DurationMS int64     `json:"duration_ms,omitempty"`
}

func (a *app) migrateUp(ctx context.Context) error {
	return a.withMigrator(ctx, func(m db.Migrator, logger *slog.Logger) error {
		results, err := m.Up(ctx)

		out := make([]migrationJSON, 0, len(results))
		for _, r := range results {
			logger.InfoContext(ctx, "migration applied", slog.String("migration", r.Name), slog.Duration("duration", r.Duration))
			out = append(out, migrationJSON{Version: r.Version, Name: r.Name, Applied: true, DurationMS: r.Duration.Milliseconds()})

			if !a.json {
				a.print("applied %s (%s)", r.Name, r.Duration.Round(time.Microsecond))
			}
		}

		if err != nil {
			return err
		}

		if a.json {
			return a.printJSON(out)
		}

		if len(results) == 0 {
			a.print("no pending migrations")
		}

		return nil
	})
}

func (a *app) migrateDown(ctx context.Context) error {
	return a.withMigrator(ctx, func(m db.Migrator, logger *slog.Logger) error {
		r, err := m.Down(ctx)
		if errors.Is(err, db.ErrNoMigration) {
			a.print("no migration to roll back")

			return nil
		}

		if err != nil {
			return err
		}

		logger.InfoContext(ctx, "migration rolled back", slog.String("migration", r.Name), slog.Duration("duration", r.Duration))

		if a.json {
			return a.printJSON(migrationJSON{Version: r.Version, Name: r.Name, DurationMS: r.Duration.Milliseconds()})
		}

		a.print("rolled back %s", r.Name)

		return nil
	})
}

func (a *app) migrateStatus(ctx context.Context) error {
	return a.withMigrator(ctx, func(m db.Migrator, _ *slog.Logger) error {
		statuses, err := m.Status(ctx)
		if err != nil {
			return err
		}

		checkErr := m.Check(ctx)

		if a.json {
			out := make([]migrationJSON, 0, len(statuses))
			for _, s := range statuses {
				out = append(out, migrationJSON{Version: s.Version, Name: s.Name, Applied: s.Applied, AppliedAt: s.AppliedAt})
			}

			return a.printJSON(out)
		}

		for _, s := range statuses {
			state := "pending"
			if s.Applied {
				state = "applied " + s.AppliedAt.UTC().Format(time.RFC3339)
			}

			a.print("%-40s %s", s.Name, state)
		}

		if checkErr != nil {
			a.print("schema: %v", checkErr)
		} else {
			a.print("schema: up to date")
		}

		return nil
	})
}
