package wire

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/config"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/infra/gateway"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/grid/infra/ratelimit"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Gateway authz rate limits (TECHNICAL_SPEC §5.12): WebSocket upgrades 10
// per minute per client address, token mints 30 per minute per session.
const (
	upgradeEvery = 6 * time.Second
	upgradeBurst = 10
	mintEvery    = 2 * time.Second
	mintBurst    = 30
)

// GatewayAvailable reports whether this build embeds the gateway: the hub
// and all roles need it.
func GatewayAvailable() bool { return gateway.Available() }

// gatewayAddr is the main public address, for logs.
func gatewayAddr(g config.Gateway) string {
	if g.TLSMode == config.TLSModeOff {
		return g.HTTPListen
	}

	return g.HTTPSListen
}

// newGateway builds the embedded gateway serving router.
func newGateway(cfg config.Hub, logger *slog.Logger, router http.Handler, g *hubGrid) (*gateway.Gateway, error) {
	gw := cfg.Gateway

	return gateway.New(gateway.Options{
		Config: gateway.Config{
			HTTPSListen: gw.HTTPSListen, HTTPListen: gw.HTTPListen, TLSMode: gw.TLSMode,
			CertFile: gw.TLSCert, KeyFile: gw.TLSKey, PublicURL: cfg.Hub.URL,
			ACMEEmail: gw.ACMEEmail, ACMECA: gw.ACMECA, StorageDir: gw.StorageDir,
			StreamCloseDelay: gw.StreamCloseDelay.Duration(), StreamTimeout: gw.StreamTimeout.Duration(),
			LogLevel: cfg.Log.Level,
		},
		Hub:     router,
		NodeTLS: g.gatewayDialConfig,
		Logger:  logger,
	})
}

// gatewayDialConfig is the TLS config of a gateway connection to a node.
func (g *hubGrid) gatewayDialConfig(ctx context.Context, nodeID string) (*tls.Config, error) {
	if g.manager == nil {
		return nil, errGridDisabled
	}

	return g.manager.GatewayDialConfig(ctx, g.gatewayClient, nodeID)
}

// mediaAccess builds the gateway forward auth of the hub.
func (g *hubGrid) mediaAccess(cfg config.Hub, logger *slog.Logger) (*gridapp.MediaAccess, error) {
	if g.ca != nil {
		g.gatewayClient = pki.NewClientSource(g.ca, pki.KindGateway, g.hubID, time.Now)
	}

	return gridapp.NewMediaAccess(gridapp.MediaAccessOptions{
		Nodes: g.nodeRepo, Devices: g.deviceRepo, Tracker: g.tracker, Presence: g.presence, Issuer: g.keys,
		Policy: listenPolicy(cfg.Settings.ListenPolicy), HubURL: cfg.Hub.URL,
		Upgrades: ratelimit.New(upgradeEvery, upgradeBurst), Mints: ratelimit.New(mintEvery, mintBurst),
		Now: time.Now, Logger: component(logger, "grid.app.authz"),
	})
}

// listenPolicy is the global listen policy until the settings store exists
// (settings.listen_policy, config or default).
type listenPolicy string

func (p listenPolicy) ListenPolicy(context.Context) string { return string(p) }

// routes is a router module that only adds routes.
type routes func(r chi.Router)

func (routes) Middlewares() []func(http.Handler) http.Handler { return nil }

func (f routes) Routes(r chi.Router) { f(r) }

// bodyLimit is a router module capping every request body at n bytes.
type bodyLimit int64

func (b bodyLimit) Middlewares() []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{httpserver.LimitBody(int64(b))}
}

func (bodyLimit) Routes(chi.Router) {}

// subject adapts an identity principal to the grid authz.
type subject struct{ p identitydomain.Principal }

func subjectOf(p identitydomain.Principal) gridapp.Subject { return subject{p: p} }

func (s subject) IsAnonymous() bool { return s.p.IsAnonymous() }

func (s subject) UserID() shared.UUID {
	u, _ := shared.ParseUUID(s.p.UserID().String())

	return u
}

func (s subject) SessionID() shared.UUID {
	u, _ := shared.ParseUUID(s.p.SessionID().String())

	return u
}

func (s subject) Roles() []string {
	roles := s.p.Roles()
	out := make([]string, 0, len(roles))

	for _, r := range roles {
		out = append(out, r.String())
	}

	return out
}

func (s subject) Role() string {
	if s.p.IsAnonymous() {
		return ""
	}

	return s.p.Role().String()
}

func (s subject) RoleRank() int { return int(s.p.Role().ID()) }

func (s subject) HasOnDevice(role, device string) bool {
	r, err := identitydomain.ParseRole(role)
	if err != nil {
		return false
	}

	d, err := identitydomain.NewDeviceID(device)
	if err != nil {
		return false
	}

	return s.p.HasOnDevice(r, d)
}
