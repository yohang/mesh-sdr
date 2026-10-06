package cli

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/pressly/goose/v3"
	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/migrations"
)

func newMigrateCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage database migrations",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "up",
			Short: "Apply all pending migrations",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return withProvider(a, func(p *goose.Provider) error {
					results, err := p.Up(cmd.Context())
					for _, r := range results {
						a.logger.Info("migration applied", slog.String("migration", r.Source.Path), slog.Duration("duration", r.Duration))
					}
					if err != nil {
						return fmt.Errorf("migrate up: %w", err)
					}
					if len(results) == 0 {
						a.logger.Info("no pending migrations")
					}
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "down",
			Short: "Roll back the latest migration",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return withProvider(a, func(p *goose.Provider) error {
					r, err := p.Down(cmd.Context())
					if errors.Is(err, goose.ErrNoNextVersion) {
						a.logger.Info("no migration to roll back")
						return nil
					}
					if err != nil {
						return fmt.Errorf("migrate down: %w", err)
					}
					a.logger.Info("migration rolled back", slog.String("migration", r.Source.Path), slog.Duration("duration", r.Duration))
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show migrations status",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return withProvider(a, func(p *goose.Provider) error {
					statuses, err := p.Status(cmd.Context())
					if err != nil {
						return fmt.Errorf("migrate status: %w", err)
					}
					for _, s := range statuses {
						a.logger.Info("migration status",
							slog.String("migration", s.Source.Path),
							slog.String("state", string(s.State)),
							slog.Time("applied_at", s.AppliedAt),
						)
					}
					return nil
				})
			},
		},
	)

	return cmd
}

func withProvider(a *app, fn func(*goose.Provider) error) error {
	conn, err := db.Open(a.cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	p, err := goose.NewProvider(goose.DialectSQLite3, conn, migrations.FS)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}

	return fn(p)
}
