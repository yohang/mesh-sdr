// Package wire is the composition root: it builds the object graph of each
// role (hub, node) from its config, a logger and, for the hub, the database
// adapter. CLI commands call it; nothing else does.
package wire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/gateway"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/mail"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func component(logger *slog.Logger, name string) *slog.Logger {
	return logger.With(slog.String("component", name))
}

// OpenDB opens the SQLite database named by db.dsn. The caller closes it.
func OpenDB(ctx context.Context, cfg config.DB, logger *slog.Logger) (*db.DB, error) {
	path, err := db.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db.dsn: %w", err)
	}

	d, err := db.Open(ctx, db.Options{
		Path:               path,
		MaxReadConnections: cfg.MaxReadConnections,
		Logger:             component(logger, "db"),
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	return d, nil
}

// Process is a role's network process: one HTTP(S) server, startup tasks
// run before serving and background workers that live as long as it. The
// hub serves through its gateway (front); Serve serves the handler directly
// on a listener (nodes, and hub tests without the gateway).
type Process struct {
	addr     string
	server   *http.Server
	front    *gateway.Gateway
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

	stop := p.runWorkers(ctx)
	defer stop()

	return httpserver.Run(ctx, p.logger, p.server, ln)
}

// runWorkers starts the workers; stop cancels them and waits for them.
func (p *Process) runWorkers(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	for _, w := range p.workers {
		wg.Go(func() { w(ctx) })
	}

	return func() {
		cancel()
		wg.Wait()
	}
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

	stop := p.runWorkers(ctx)
	defer stop()

	if err := p.front.Start(ctx); err != nil {
		return err
	}

	<-ctx.Done()

	p.logger.InfoContext(ctx, "stopping the gateway")

	return p.front.Stop()
}

// IdentityDeps returns the dependencies of the identity module backed by
// adapter (the hub, and the CLI user administration): a deleted user also
// leaves the grid presence registry (SR-64).
func IdentityDeps(cfg config.Hub, logger *slog.Logger, adapter *db.DB) identity.Deps {
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
	return c.repo.EraseUser(ctx, id.UUID())
}

// mailQueue returns the outgoing mail queue, or nil when smtp.host is not
// set.
func mailQueue(ctx context.Context, cfg config.SMTP, logger *slog.Logger) *mail.Queue {
	if !cfg.Enabled() {
		logger.InfoContext(ctx, "mail is not configured (smtp.host): links are shown to copy, password reset by e-mail is off")

		return nil
	}

	if cfg.TLS == string(mail.TLSNone) {
		logger.WarnContext(ctx, "smtp.tls = none: mail and SMTP credentials travel in clear; use this only with a local development relay",
			slog.String("smtp_host", cfg.Host))
	}

	return mail.NewQueue(mail.NewSMTP(mail.SMTPConfig{
		Host: cfg.Host, Port: cfg.Port, TLS: mail.TLSMode(cfg.TLS), Username: cfg.Username,
		Password: cfg.Password.Reveal(), From: cfg.From,
	}), component(logger, "mail.queue"))
}

// Hub builds the hub: web UI and REST API behind the embedded gateway,
// backed by adapter, the session reaper and the grid. The caller checks the schema version before (see
// db.Migrator.Check).
func Hub(ctx context.Context, cfg config.Hub, origins config.Origins, logger *slog.Logger, adapter *db.DB) (*Process, error) {
	p, _, err := newHub(ctx, cfg, origins, logger, adapter, time.Now, gridapp.DefaultTimings())

	return p, err
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

	_, cert, err := selfSigned(cfg, id, now)
	if err != nil {
		return nil, err
	}

	return preEnrollment(cfg.Node.Listen, cert, nil, logger), nil
}

// gridDevices adapts the grid device registry to the token issuer.
type gridDevices struct{ devices *gridapp.Devices }

func (g gridDevices) NodeDevices(ctx context.Context, nodeID string) ([]identityapp.NodeDevice, error) {
	id, err := griddomain.NewNodeID(nodeID)
	if err != nil {
		return nil, nil //nolint:nilerr // not a node id: no device
	}

	devices, err := g.devices.ListByNode(ctx, id)
	if err != nil {
		return nil, err
	}

	var out []identityapp.NodeDevice

	for _, d := range devices {
		if !d.Flags().Enabled {
			continue
		}

		out = append(out, identityapp.NodeDevice{
			ID: d.ID().String(), ListenPolicy: identitydomain.ListenPolicy(d.Flags().ListenPolicy),
			OperatorCanRetune: d.Flags().OperatorCanRetune,
		})
	}

	return out, nil
}

// userNames gives the names of users to the grid admin pages.
type userNames struct{ users identitydomain.UserRepository }

// Names implements gridhttp.UserNames: the display name, else the username.
func (u userNames) Names(ctx context.Context, ids []shared.UUID) (map[shared.UUID]string, error) {
	out := make(map[shared.UUID]string, len(ids))

	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}

		uid, err := identitydomain.NewUserID(id)
		if err != nil {
			continue
		}

		user, err := u.users.ByID(ctx, uid)
		if errors.Is(err, identitydomain.ErrUserNotFound) {
			continue
		}

		if err != nil {
			return nil, err
		}

		out[id] = user.Name()
	}

	return out, nil
}
