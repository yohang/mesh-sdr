// Package wire is the composition root: it builds the object graph of each
// role (hub, node) from its config, a logger and, for the hub, the database
// adapter. CLI commands call it; nothing else does.
package wire

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
	gridinfra "github.com/yohang/mesh-sdr/internal/grid/infra"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/http/api"
)

func component(logger *slog.Logger, name string) *slog.Logger {
	return logger.With(slog.String("component", name))
}

// OpenDB opens the adapter selected by the db.dsn scheme. The caller closes it.
func OpenDB(ctx context.Context, cfg config.DB, logger *slog.Logger) (db.Adapter, error) {
	dsn, err := db.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db.dsn: %w", err)
	}

	switch dsn.Dialect() {
	case db.DialectSQLite:
		a, err := sqlite.Open(ctx, sqlite.Options{
			Path:               dsn.Path(),
			MaxReadConnections: cfg.MaxReadConnections,
			Logger:             component(logger, "db.sqlite.adapter"),
		})
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}

		return a, nil
	default:
		return nil, fmt.Errorf("db.dsn: %w: %s", db.ErrEngineUnsupported, dsn.Dialect())
	}
}

// Process is a role's network process: one HTTP(S) server.
type Process struct {
	addr   string
	server *http.Server
	logger *slog.Logger
}

// Addr returns the configured listen address.
func (p *Process) Addr() string { return p.addr }

// Listen opens the listening socket. The address decides IPv4 or IPv6: an
// IPv4 literal (0.0.0.0) listens on IPv4 only, an IPv6 literal ([::]) on
// IPv6, a host name on both.
func (p *Process) Listen(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig

	network := "tcp"

	if host, _, err := net.SplitHostPort(p.addr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			network = "tcp6"
			if ip.To4() != nil {
				network = "tcp4"
			}
		}
	}

	ln, err := lc.Listen(ctx, network, p.addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", p.addr, err)
	}

	return ln, nil
}

// Serve serves on ln until ctx is done, then shuts down gracefully.
func (p *Process) Serve(ctx context.Context, ln net.Listener) error {
	return httpserver.Run(ctx, p.logger, p.server, ln)
}

// Run listens and serves until ctx is done.
func (p *Process) Run(ctx context.Context) error {
	ln, err := p.Listen(ctx)
	if err != nil {
		return err
	}

	return p.Serve(ctx, ln)
}

// Hub builds the hub: web UI and REST API on hub.listen, backed by adapter.
// The caller checks the schema version before (see db.Migrator.Check).
func Hub(cfg config.Hub, logger *slog.Logger, adapter db.Adapter) *Process {
	apiServer := api.Server{
		HealthHandlers: api.NewHealthHandlers(adapter, component(logger, "http.api.health")),
	}

	router := httpserver.NewRouter(
		component(logger, "http.router"),
		api.NewHandler(apiServer, component(logger, "http.api")),
	)

	return &Process{
		addr:   cfg.Hub.Listen,
		server: httpserver.NewServer(cfg.Hub.Listen, router),
		logger: component(logger, "http.server"),
	}
}

// Node builds a node. Until enrollment exists (GRID-006/GRID-007) the node
// serves only the pre-enrollment API, over TLS 1.3 with an ephemeral
// self-signed certificate generated at start.
func Node(cfg config.Node, logger *slog.Logger, now time.Time) (*Process, error) {
	id, err := griddomain.NewNodeID(cfg.Node.ID)
	if err != nil {
		return nil, fmt.Errorf("node.id: %w", err)
	}

	cert, err := gridinfra.SelfSignedCertificate(id, cfg.Node.Listen, now)
	if err != nil {
		return nil, err
	}

	srv := httpserver.NewServer(cfg.Node.Listen, gridhttp.NewPreEnrollmentRouter(component(logger, "grid.http.enrollment")))
	srv.TLSConfig = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}

	return &Process{addr: cfg.Node.Listen, server: srv, logger: component(logger, "grid.http.server")}, nil
}
