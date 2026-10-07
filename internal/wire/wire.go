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

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/sqlite"
	fileshttp "github.com/yohang/mesh-sdr/internal/files/http"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/grid/infra/gateway"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/http/clientip"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
	"github.com/yohang/mesh-sdr/internal/identity/infra/keyring"
	"github.com/yohang/mesh-sdr/internal/identity/infra/settingsrc"
	identitysqlite "github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/mail"
	"github.com/yohang/mesh-sdr/internal/settings"
	settingsapp "github.com/yohang/mesh-sdr/internal/settings/app"
	settingshttp "github.com/yohang/mesh-sdr/internal/settings/http"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shell"
	"github.com/yohang/mesh-sdr/internal/web/layout"
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

// Front owns the public listeners of a process (the hub gateway).
type Front interface {
	Start(ctx context.Context) error
	Stop() error
}

// Process is a role's network process: one HTTP(S) server, startup tasks
// run before serving and background workers that live as long as it. The
// hub serves through its gateway (front); Serve serves the handler directly
// on a listener (nodes, and hub tests without the gateway).
type Process struct {
	addr     string
	server   *http.Server
	front    Front
	logger   *slog.Logger
	startup  []func(ctx context.Context) error
	workers  []func(ctx context.Context)
	setupURL string
	// started is set once the startup tasks ran.
	started bool
}

// runStartup runs the startup tasks once.
func (p *Process) runStartup(ctx context.Context) error {
	if p.started {
		return nil
	}

	for _, task := range p.startup {
		if err := task(ctx); err != nil {
			return err
		}
	}

	p.started = true

	return nil
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
	if err := p.runStartup(ctx); err != nil {
		_ = ln.Close()

		return err
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

// Run serves until ctx is done: through the front when the process has
// one, otherwise on its listen address.
func (p *Process) Run(ctx context.Context) error {
	if p.front != nil {
		return p.runFront(ctx)
	}

	ln, err := p.Listen(ctx)
	if err != nil {
		return err
	}

	return p.Serve(ctx, ln)
}

// runFront runs the startup tasks and the workers, starts the front, and
// stops it when ctx is done.
func (p *Process) runFront(ctx context.Context) error {
	if err := p.runStartup(ctx); err != nil {
		return err
	}

	wctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	for _, w := range p.workers {
		wg.Go(func() { w(wctx) })
	}

	err := p.front.Start(ctx)
	if err == nil {
		<-ctx.Done()

		p.logger.InfoContext(ctx, "stopping the gateway")
		err = p.front.Stop()
	}

	cancel()
	wg.Wait()

	return err
}

// identityDeps returns the dependencies of the identity module.
func identityDeps(cfg config.Hub, logger *slog.Logger, adapter db.Adapter) identity.Deps {
	return identity.Deps{
		Config: cfg, Logger: logger, DB: adapter, IDs: shared.NewUUIDv7Generator(), Now: time.Now,
		Erasers: []identityapp.UserEraser{connectionEraser{gridsqlite.NewConnectionRepository(adapter)}},
	}
}

// connectionEraser removes a deleted user from the grid presence registry
// (SR-64).
type connectionEraser struct {
	repo *gridsqlite.ConnectionRepository
}

func (c connectionEraser) EraseUser(ctx context.Context, id identitydomain.UserID) error {
	u, err := shared.UUIDFromBytes(id.Bytes())
	if err != nil {
		return err
	}

	return c.repo.EraseUser(ctx, u)
}

// mailQueue returns the outgoing mail queue, or nil when smtp.host is not
// set.
func mailQueue(cfg config.SMTP, logger *slog.Logger) *mail.Queue {
	if !cfg.Enabled() {
		logger.Info("mail is not configured (smtp.host): links are shown to copy, password reset by e-mail is off")

		return nil
	}

	if cfg.TLS == string(mail.TLSNone) {
		logger.Warn("smtp.tls = none: mail and SMTP credentials travel in clear; use this only with a local development relay",
			slog.String("smtp_host", cfg.Host))
	}

	return mail.NewQueue(mail.NewSMTP(mail.SMTPConfig{
		Host: cfg.Host, Port: cfg.Port, TLS: mail.TLSMode(cfg.TLS), Username: cfg.Username,
		Password: cfg.Password.Reveal(), From: cfg.From,
	}), component(logger, "mail.queue"))
}

// UserAdmin builds the user administration service of the hub CLI
// (meshsdr hub user …), backed by adapter.
func UserAdmin(cfg config.Hub, logger *slog.Logger, adapter db.Adapter) *identityapp.UserAdmin {
	return identity.UserAdmin(identityDeps(cfg, logger, adapter))
}

// Hub builds the hub: web UI and REST API behind the embedded gateway,
// backed by adapter, the session reaper and the grid. The caller checks the schema version before (see
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
	images := branding(adapter, auditLog)

	// The top bar shows the signed-in user: the identity module, built
	// after the shell (it renders its pages with the shell), fills it in.
	var identityHTTP *identityhttp.Module

	userOf := func(r *http.Request) *layout.User {
		if identityHTTP == nil {
			return nil
		}

		return identityHTTP.ShellUser(r)
	}

	settingsModule, err := settings.Wire(ctx, settings.Deps{
		Config: cfg, Origins: origins, DB: adapter, Now: now, Logger: logger,
		Audit: settingsAuditor{log: auditLog},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("settings: %w", err)
	}

	adminGate := &roleGate{role: identitydomain.RoleAdmin}
	shellModule := shell.Wire(shell.Deps{
		Settings: settingsModule.Store, AdminGate: adminGate, User: userOf, Logger: logger,
		Images: stationImages{b: images, logger: component(logger, "shell.infra.station_images")},
	})

	ideps := identityDeps(cfg, logger, adapter)
	ideps.Settings = settingsModule.Store
	ideps.AcceptsMultipart = api.AcceptsMultipart
	ideps.Devices = gridDevices{g.devices}

	var workers []func(context.Context)

	if q := mailQueue(cfg.SMTP, logger); q != nil {
		ideps.Mail = q
		workers = append(workers, q.Run)
	}

	// Grid ↔ identity (ACC-007, GRID-011/012): revoked sessions and users
	// go to the nodes, and POST /auth/token refreshes only connections the
	// gateway authz issued to the caller.
	if g.manager != nil {
		ideps.Revocations = nodeRevocations{b: g.manager, now: now}
	}

	ideps.Binder = connectionBinder{repo: gridsqlite.NewConnectionRepository(adapter)}

	idm, err := identity.Wire(ctx, ideps, pages{shellModule.Renderer})
	if err != nil {
		return nil, nil, fmt.Errorf("identity: %w", err)
	}

	g.keys.attach(idm.Keys)

	workers = append(workers, func(ctx context.Context) {
		idm.RunKeyMaintenance(ctx, now, component(logger, "identity.infra.keyring"))
	})

	adminGate.authz = idm.HTTP
	identityHTTP = idm.HTTP

	// Presets, schedules and reporting (ADR 0020), wired to the grid.
	sch := newScheduling(adapter, g, settingsModule.Store, auditLog, now, logger)

	if g.states != nil {
		// The desired state carries listen_policy: a settings change is
		// pushed to the nodes by one worker that coalesces the changes and
		// stops with the hub.
		changed := make(chan struct{}, 1)

		settingsModule.Store.Subscribe(func(*settingsapp.Snapshot) {
			select {
			case changed <- struct{}{}:
			default:
			}
		})

		statesLogger := component(logger, "grid.app.states")
		workers = append(workers, func(ctx context.Context) {
			for {
				select {
				case <-ctx.Done():
					return
				case <-changed:
					if n := g.states.PublishAll(ctx); n > 0 {
						statesLogger.DebugContext(ctx, "desired state pushed after a settings change", slog.Int("nodes", n))
					}
				}
			}
		})
	}

	scheduler, retention, err := jobs(adapter, idm, sch, settingsModule.Store, auditLog, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("jobs: %w", err)
	}

	imagesHTTP := fileshttp.New(images, idm.HTTP.Require(identitydomain.RoleAdmin), filesActor,
		shellModule.Renderer.Error, component(logger, "files.http"))
	access, err := g.mediaAccess(cfg, listenPolicy{settingsrc.New(settingsModule.Store, component(logger, "grid.infra.settings"))}, logger)
	if err != nil {
		return nil, nil, err
	}

	authz := gridhttp.NewAuthzHandler(access, func(r *http.Request) gridapp.Subject { return subjectOf(idm.HTTP.Principal(r.Context())) },
		func(r *http.Request) string { return clientip.From(r.Context()).String() }, component(logger, "grid.http.authz"))

	apiServer := api.Server{
		HealthHandlers:     api.NewHealthHandlers(adapter, component(logger, "http.api.health")),
		AuthHandlers:       api.NewAuthHandlers(idm.HTTP),
		GridHandlers:       api.NewGridHandlers(idm.HTTP, g.nodes, g.history, g.caps, g.devices, g.presence),
		SettingsHandlers:   api.NewSettingsHandlers(settingsModule.Store, settingsModule.Effective, settingsActor),
		RetentionHandlers:  api.NewRetentionHandlers(retention, settingsActor),
		BrandingHandlers:   api.NewBrandingHandlers(images, settingsActor),
		AccountHandlers:    api.NewAccountHandlers(idm.HTTP, idm.Accounts, idm.Profile),
		InvitationHandlers: api.NewInvitationHandlers(idm.HTTP, idm.HTTP, idm.Invitations),
		ResetHandlers:      api.NewResetHandlers(idm.HTTP, idm.Resets),
		AuditHandlers:      api.NewAuditHandlers(idm.Audit),
		TokenHandlers:      api.NewTokenHandlers(idm.HTTP, idm.HTTP, idm.Tokens),
		FeatureHandlers: api.NewFeatureHandlers(idm.HTTP, gridapp.NewFeatures(gridsqlite.NewDeviceRepository(adapter),
			gridsqlite.NewCapabilityRepository(adapter), storeListenPolicy{store: settingsModule.Store})),
		PresetHandlers:    api.NewPresetHandlers(sch.presets, scheduleDevices{repo: g.deviceRepo}),
		ScheduleHandlers:  api.NewScheduleHandlers(sch.schedules, deviceScope{}),
		ReportingHandlers: api.NewReportingHandlers(sch.reporting),
	}

	router := httpserver.NewRouter(
		component(logger, "http.router"),
		api.NewHandler(apiServer, idm.HTTP, component(logger, "http.api")),
		// Every request body, API and forms, is capped (gateway.max_body)
		// before any module reads it.
		bodyLimit(cfg.Gateway.MaxBody.Bytes()),
		idm.HTTP,
		settingshttp.New(settingshttp.Deps{
			Render: shellModule.Renderer, Guard: idm.HTTP.Require(identitydomain.RoleAdmin),
			Store: settingsModule.Store, Config: settingsModule.Effective, Retention: retentionRows{r: retention},
			Actor: settingsActor, Images: imagesHTTP, Schedules: deviceSchedules{schedules: sch.schedules, presets: sch.presets},
			Logger: component(logger, "settings.http"),
		}),
		imagesHTTP,
		gridhttp.NewAdminModule(gridhttp.AdminDeps{
			Render: shellModule.Renderer, Devices: g.devices, Nodes: g.nodes,
			Schedules: deviceSchedules{schedules: sch.schedules, presets: sch.presets},
			Operator:  idm.HTTP.Require(identitydomain.RoleOperator), Admin: idm.HTTP.Require(identitydomain.RoleAdmin),
			IsAdmin: func(r *http.Request) bool { return adminGate.Allows(r.Context()) }, Logger: component(logger, "grid.http.admin"),
		}),
		routes(func(r chi.Router) { r.Method(http.MethodGet, gateway.AuthzPath, authz) }),
		shellModule.HTTP,
	)

	setupURL, err := idm.Setup.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("identity setup: %w", err)
	}

	front, err := newGateway(cfg, logger, router, g)
	if err != nil {
		return nil, nil, err
	}

	return &Process{
		addr:     gatewayAddr(cfg.Gateway),
		server:   httpserver.NewServer("", router),
		front:    front,
		logger:   component(logger, "http.server"),
		startup:  g.startup,
		workers:  append(append(workers, scheduler.Run, sch.reporting.Run), g.workers...),
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

// gridDevices adapts the grid device registry to the token issuer.
type gridDevices struct{ devices *gridapp.Devices }

func (g gridDevices) NodeDevices(ctx context.Context, nodeID string) ([]identityapp.NodeDevice, error) {
	all, err := g.devices.List(ctx)
	if err != nil {
		return nil, err
	}

	var out []identityapp.NodeDevice

	for _, d := range all {
		if d.Node().String() != nodeID || !d.Flags().Enabled {
			continue
		}

		out = append(out, identityapp.NodeDevice{
			ID: d.ID().String(), ListenPolicy: identitydomain.ListenPolicy(d.Flags().ListenPolicy),
			OperatorCanRetune: d.Flags().OperatorCanRetune,
		})
	}

	return out, nil
}

// TokenKeyring opens the token signing keyring (meshsdr hub keys …).
func TokenKeyring(cfg config.Hub, now time.Time) (*keyring.Keyring, error) {
	return identity.Keyring(cfg, now)
}
