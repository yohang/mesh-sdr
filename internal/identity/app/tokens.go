package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// TokenSigner gives the key that signs at now (the keyring).
type TokenSigner interface {
	Signer(now time.Time) (ed25519.PrivateKey, string, error)
}

// KeySource is the public side of the keyring that nodes need: the keys to
// accept and the revoked kids, and a notification when they change. Grid
// pushes them with ctl.keys.update; GET /.well-known/jwks.json serves them.
type KeySource interface {
	Published(now time.Time) (token.JWKS, []string)
	OnChange(f func())
}

// NodeDevice is what the issuer needs to know of a device to scope a
// token: its effective listen policy and retune flag (§5.9, §5.11).
type NodeDevice struct {
	ID string
	// ListenPolicy is the device's own policy; empty means the global one.
	ListenPolicy      domain.ListenPolicy
	OperatorCanRetune bool
}

// DeviceAccess lists the devices of a node (the grid device registry).
type DeviceAccess interface {
	NodeDevices(ctx context.Context, nodeID string) ([]NodeDevice, error)
}

// ConnectionBinder checks that a media connection id was issued to the
// caller (the gateway forward auth, GRID-011). Until it exists, signed-in
// callers bind their tokens with sid and sub, and anonymous tokens are
// refused.
type ConnectionBinder interface {
	Bound(ctx context.Context, cid, nodeID string, by Actor) (bool, error)
}

// DefaultMaxDemods is the lim.max_demods claim until the setting exists.
const DefaultMaxDemods = 4

var connectionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var nodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// Tokens issues the access tokens that the browser presents to nodes
// (ACC-007, TECHNICAL_SPEC §5.8): short-lived EdDSA JWS carrying the user
// (or anon), roles and device scopes, verified offline by nodes. They are
// derived from the live session at each refresh: a revoked session or a
// changed role takes effect within one TTL, at once with ctl.revocations.
type Tokens struct {
	signer   TokenSigner
	devices  DeviceAccess
	binder   ConnectionBinder
	settings Settings
	limiter  KeyLimiter
	issuer   string
	ttl      time.Duration
	now      Clock
	logger   *slog.Logger
}

// TokensDeps are the dependencies of Tokens.
type TokensDeps struct {
	Signer   TokenSigner
	Devices  DeviceAccess
	Binder   ConnectionBinder // optional
	Settings Settings
	// Limiter limits mints per session (or connection): 30/min (§5.12).
	Limiter KeyLimiter
	// Issuer is hub.url (the iss claim).
	Issuer string
	TTL    time.Duration
	Now    Clock
	Logger *slog.Logger
}

// NewTokens returns the issuer.
func NewTokens(d TokensDeps) *Tokens {
	return &Tokens{
		signer: d.Signer, devices: d.Devices, binder: d.Binder, settings: d.Settings, limiter: d.Limiter,
		issuer: d.Issuer, ttl: d.TTL, now: d.Now, logger: d.Logger,
	}
}

// MintInput asks for a token for one media connection.
type MintInput struct {
	By Actor
	// SessionRef is Session.Ref of the request's session ("" when
	// anonymous): the sid claim.
	SessionRef   string
	NodeID       string
	ConnectionID string
}

// Minted is an issued token. Raw must never be logged or stored (§5.8).
type Minted struct {
	Raw       string
	ExpiresAt time.Time
	Claims    token.Claims
}

// Mint issues a token for a node and a connection: scoped to the devices of
// the node the caller may listen to, with the operator permissions it holds
// there.
func (s *Tokens) Mint(ctx context.Context, in MintInput) (Minted, error) {
	p := in.By.Principal

	if !nodePattern.MatchString(in.NodeID) {
		return Minted{}, domain.ErrNoListenableDevice
	}

	if !connectionPattern.MatchString(in.ConnectionID) {
		return Minted{}, domain.ErrInvalidConnection
	}

	if p.IsAnonymous() && s.binder == nil {
		return Minted{}, domain.ErrAnonymousTokens
	}

	key := "cid:" + in.ConnectionID
	if !p.IsAnonymous() {
		key = "sid:" + in.SessionRef
	}

	now := s.now()

	if s.limiter != nil {
		if ok, wait := s.limiter.Allow(key, now); !ok {
			return Minted{}, domain.NewRateLimitError(wait)
		}
	}

	if s.binder != nil {
		ok, err := s.binder.Bound(ctx, in.ConnectionID, in.NodeID, in.By)
		if err != nil {
			return Minted{}, fmt.Errorf("check connection: %w", err)
		}

		if !ok {
			return Minted{}, domain.ErrInvalidConnection
		}
	}

	devices, err := s.devices.NodeDevices(ctx, in.NodeID)
	if err != nil {
		return Minted{}, fmt.Errorf("list devices of node %s: %w", in.NodeID, err)
	}

	scopes := Scopes(p, devices, s.settings.ListenPolicy(ctx))
	if len(scopes) == 0 {
		return Minted{}, domain.ErrNoListenableDevice
	}

	c := token.Claims{
		Issuer: s.issuer, Audience: token.Audience(in.NodeID), Subject: token.AnonymousSubject,
		ConnectionID: in.ConnectionID, Roles: []string{}, Scopes: scopes, Limits: token.Limits{MaxDemods: DefaultMaxDemods},
		IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(s.ttl), ID: randomID(),
	}

	if !p.IsAnonymous() {
		c.Subject, c.SessionID = p.UserID().String(), in.SessionRef
		for _, r := range p.Roles() {
			c.Roles = append(c.Roles, r.String())
		}
	}

	priv, kid, err := s.signer.Signer(now)
	if err != nil {
		return Minted{}, fmt.Errorf("signing key: %w", err)
	}

	raw, err := token.Sign(c, priv)
	if err != nil {
		return Minted{}, fmt.Errorf("sign token: %w", err)
	}

	s.logger.DebugContext(ctx, "access token issued", slog.String("node_id", in.NodeID), slog.String("kid", kid),
		slog.Int("devices", len(scopes)), slog.Bool("anonymous", p.IsAnonymous()))

	return Minted{Raw: raw, ExpiresAt: c.ExpiresAt, Claims: c}, nil
}

// Scopes computes the scp claim: for each device the principal may listen
// to (its own policy, else global), listen and demod; preset for an
// operator on the device; retune for an admin, or an operator when the
// device allows it (§5.10, §5.11).
func Scopes(p domain.Principal, devices []NodeDevice, global domain.ListenPolicy) []token.Scope {
	out := []token.Scope{}

	for _, d := range devices {
		policy := d.ListenPolicy
		if policy == "" {
			policy = global
		}

		if !p.CanListen(policy) {
			continue
		}

		dev, err := shared.NewDeviceID(d.ID)
		if err != nil {
			continue
		}

		perms := []string{token.PermListen, token.PermDemod}

		if p.HasOnDevice(domain.RoleOperator, dev) {
			perms = append(perms, token.PermPreset)

			if d.OperatorCanRetune || p.Has(domain.RoleAdmin) {
				perms = append(perms, token.PermRetune)
			}
		}

		out = append(out, token.Scope{Device: d.ID, Perms: perms})
	}

	return out
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)

	return base64.RawURLEncoding.EncodeToString(b)
}
