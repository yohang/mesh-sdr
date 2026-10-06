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
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shell"
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

// Process is a role's network process: one HTTP(S) server, startup tasks
// run before serving and background workers that live as long as it.
type Process struct {
	addr    string
	server  *http.Server
	logger  *slog.Logger
	startup []func(ctx context.Context) error
	workers []func(ctx context.Context)
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

// Serve runs the startup tasks, starts the workers, then serves on ln
// until ctx is done, shuts down gracefully and waits for the workers.
func (p *Process) Serve(ctx context.Context, ln net.Listener) error {
	for _, task := range p.startup {
		if err := task(ctx); err != nil {
			_ = ln.Close()

			return err
		}
	}

	wctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	for _, w := range p.workers {
		wg.Go(func() { w(wctx) })
	}

	err := httpserver.Run(ctx, p.logger, p.server, ln)

	cancel()
	wg.Wait()

	return err
}

// Run listens and serves until ctx is done.
func (p *Process) Run(ctx context.Context) error {
	ln, err := p.Listen(ctx)
	if err != nil {
		return err
	}

	return p.Serve(ctx, ln)
}

// identityDeps returns the dependencies of the identity module.
func identityDeps(cfg config.Hub, logger *slog.Logger, adapter db.Adapter) identity.Deps {
	return identity.Deps{Config: cfg, Logger: logger, DB: adapter, IDs: shared.NewUUIDv7Generator(), Now: time.Now}
}

// UserAdmin builds the user administration service of the hub CLI
// (meshsdr hub user …), backed by adapter.
func UserAdmin(cfg config.Hub, logger *slog.Logger, adapter db.Adapter) *identityapp.UserAdmin {
	return identity.UserAdmin(identityDeps(cfg, logger, adapter))
}

// Hub builds the hub: web UI and REST API on hub.listen, backed by adapter,
// the session reaper and the grid. The caller checks the schema version before (see
// db.Migrator.Check).
func Hub(ctx context.Context, cfg config.Hub, logger *slog.Logger, adapter db.Adapter) (*Process, error) {
	g, err := newHubGrid(cfg, logger, adapter, time.Now)
	if err != nil {
		return nil, err
	}

	shellModule := shell.Wire(shell.Deps{Settings: cfg.Settings, Logger: logger})

	idm, err := identity.Wire(ctx, identityDeps(cfg, logger, adapter), pages{shellModule.Renderer})
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}

	apiServer := api.Server{
		HealthHandlers: api.NewHealthHandlers(adapter, component(logger, "http.api.health")),
		AuthHandlers:   api.NewAuthHandlers(idm.HTTP),
		GridHandlers:   api.NewGridHandlers(g.nodes, g.history),
	}

	router := httpserver.NewRouter(
		component(logger, "http.router"),
		api.NewHandler(apiServer, idm.HTTP, component(logger, "http.api")),
		idm.HTTP,
		shellModule.HTTP,
	)

	return &Process{
		addr:    cfg.Hub.Listen,
		server:  httpserver.NewServer(cfg.Hub.Listen, router),
		logger:  component(logger, "http.server"),
		startup: g.startup,
		workers: append([]func(context.Context){func(ctx context.Context) { idm.Reaper.Run(ctx, identityapp.SessionReapEvery) }}, g.workers...),
	}, nil
}

// Node builds a node. Until enrollment exists (GRID-006/GRID-007) the node
// serves only the pre-enrollment API, over TLS 1.3 with an ephemeral
// self-signed certificate generated at start.
func Node(cfg config.Node, logger *slog.Logger, now time.Time) (*Process, error) {
	id, err := griddomain.NewNodeID(cfg.Node.ID)
	if err != nil {
		return nil, fmt.Errorf("node.id: %w", err)
	}

	key, err := pki.GenerateKey()
	if err != nil {
		return nil, err
	}

	cert, err := pki.SelfSigned(key, id.String(), cfg.Node.Listen, now)
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
