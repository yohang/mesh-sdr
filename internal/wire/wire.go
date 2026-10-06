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
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identitysqlite "github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/settings"
	settingshttp "github.com/yohang/mesh-sdr/internal/settings/http"
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
	addr     string
	server   *http.Server
	logger   *slog.Logger
	startup  []func(ctx context.Context) error
	workers  []func(ctx context.Context)
	setupURL string
}

// Addr returns the configured listen address.
func (p *Process) Addr() string { return p.addr }

// SetupURL returns the one-time URL that creates the first admin when the
// hub has no admin (AUTH-018), or "". The caller prints it once, on stderr
// and not through the logger, since it carries a secret token.
func (p *Process) SetupURL() string { return p.setupURL }

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
func Hub(ctx context.Context, cfg config.Hub, origins config.Origins, logger *slog.Logger, adapter db.Adapter) (*Process, error) {
	p, _, err := newHub(ctx, cfg, origins, logger, adapter, time.Now, gridapp.DefaultTimings())

	return p, err
}

func newHub(ctx context.Context, cfg config.Hub, origins config.Origins, logger *slog.Logger, adapter db.Adapter,
	now func() time.Time, timings gridapp.Timings, tweaks ...func(*control.HubOptions),
) (*Process, *hubGrid, error) {
	g, err := newHubGrid(cfg, logger, adapter, now, timings, tweaks...)
	if err != nil {
		return nil, nil, err
	}

	auditLog := identitysqlite.NewAuditLog(adapter)

	settingsModule, err := settings.Wire(ctx, settings.Deps{
		Config: cfg, Origins: origins, DB: adapter, Now: now, Logger: logger,
		Audit: settingsAuditor{log: auditLog},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("settings: %w", err)
	}

	viewer := &adminViewer{}
	shellModule := shell.Wire(shell.Deps{Settings: settingsModule.Store, Viewer: viewer, Logger: logger})

	ideps := identityDeps(cfg, logger, adapter)
	ideps.Settings = settingsModule.Store

	idm, err := identity.Wire(ctx, ideps, pages{shellModule.Renderer})
	if err != nil {
		return nil, nil, fmt.Errorf("identity: %w", err)
	}

	viewer.authz = idm.HTTP

	scheduler, retention, err := jobs(adapter, idm, settingsModule.Store, auditLog, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("jobs: %w", err)
	}

	apiServer := api.Server{
		HealthHandlers:    api.NewHealthHandlers(adapter, component(logger, "http.api.health")),
		AuthHandlers:      api.NewAuthHandlers(idm.HTTP),
		GridHandlers:      api.NewGridHandlers(idm.HTTP, g.nodes, g.history, g.caps, g.devices, g.presence),
		SettingsHandlers:  api.NewSettingsHandlers(settingsModule.Store, settingsModule.Effective, settingsActor),
		RetentionHandlers: api.NewRetentionHandlers(retention, settingsActor),
	}

	router := httpserver.NewRouter(
		component(logger, "http.router"),
		api.NewHandler(apiServer, idm.HTTP, component(logger, "http.api")),
		idm.HTTP,
		settingshttp.New(settingshttp.Deps{
			Render: shellModule.Renderer, Guard: idm.HTTP.Require(identitydomain.RoleAdmin),
			Store: settingsModule.Store, Config: settingsModule.Effective, Retention: retentionRows{r: retention},
			Actor: settingsActor, Logger: component(logger, "settings.http"),
		}),
		shellModule.HTTP,
	)

	setupURL, err := idm.Setup.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("identity setup: %w", err)
	}

	return &Process{
		addr:     cfg.Hub.Listen,
		server:   httpserver.NewServer(cfg.Hub.Listen, router),
		logger:   component(logger, "http.server"),
		startup:  g.startup,
		workers:  append([]func(context.Context){scheduler.Run}, g.workers...),
		setupURL: setupURL,
	}, g, nil
}

// Node builds a node. An enrolled node (tls.cert present) serves the mTLS
// node API and /control; otherwise it serves only the pre-enrollment API,
// over TLS 1.3 with an ephemeral self-signed certificate.
func Node(cfg config.Node, logger *slog.Logger, now time.Time, opts ...NodeOption) (*Process, error) {
	id, err := griddomain.NewNodeID(cfg.Node.ID)
	if err != nil {
		return nil, fmt.Errorf("node.id: %w", err)
	}

	if NodeEnrolled(cfg) {
		return enrolledNode(cfg, id, logger, opts...)
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
