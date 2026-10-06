package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/db"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
)

const shutdownTimeout = 10 * time.Second

func newServeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			conn, err := db.Open(a.cfg.DBPath)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()

			srv := httpserver.NewServer(a.cfg.HTTPAddr, httpserver.NewRouter(a.logger, conn))

			errCh := make(chan error, 1)
			go func() {
				a.logger.Info("http server listening", slog.String("addr", a.cfg.HTTPAddr))
				errCh <- srv.ListenAndServe()
			}()

			select {
			case err := <-errCh:
				if !errors.Is(err, http.ErrServerClosed) {
					return fmt.Errorf("http server: %w", err)
				}
				return nil
			case <-ctx.Done():
			}

			a.logger.Info("shutting down http server")

			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()

			if err := srv.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("http server shutdown: %w", err)
			}

			return nil
		},
	}
}
