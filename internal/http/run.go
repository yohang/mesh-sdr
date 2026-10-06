package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ShutdownTimeout bounds the graceful shutdown of a server.
const ShutdownTimeout = 10 * time.Second

// Run serves srv on ln until ctx is done, then shuts it down gracefully.
// When srv.TLSConfig is set, it serves TLS with the configured certificates.
func Run(ctx context.Context, logger *slog.Logger, srv *http.Server, ln net.Listener) error {
	errCh := make(chan error, 1)

	go func() {
		logger.InfoContext(ctx, "http server listening",
			slog.String("addr", ln.Addr().String()), slog.Bool("tls", srv.TLSConfig != nil))

		if srv.TLSConfig != nil {
			errCh <- srv.ServeTLS(ln, "", "")
		} else {
			errCh <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("http server %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}

	logger.InfoContext(ctx, "shutting down http server", slog.String("addr", ln.Addr().String()))

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http server %s shutdown: %w", ln.Addr(), err)
	}

	return nil
}
