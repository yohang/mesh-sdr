package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Role names carried in access tokens.
const (
	RoleListener = "listener"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// AccessTokenTTL is the access-token lifetime until auth.token_ttl exists
// (§5.8, ADR 0011: 5 minutes).
const AccessTokenTTL = 5 * time.Minute

// Demodulators per connection by role (§6.9).
var maxDemods = map[string]int{"": 1, RoleListener: 2, RoleOperator: 4, RoleAdmin: 4}

// Subject is who asks for a media connection, resolved from the session.
type Subject interface {
	IsAnonymous() bool
	UserID() shared.UUID
	SessionID() shared.UUID
	// Roles returns the role names held globally or on some device.
	Roles() []string
	// Role returns the highest global role name ("" when anonymous).
	Role() string
	// RoleRank is the rank of Role (roles.id: 0, 10, 20, 30).
	RoleRank() int
	// HasOnDevice reports whether the subject holds role on device,
	// globally or through a device-scoped grant.
	HasOnDevice(role, device string) bool
}

// RateLimiter is a token bucket per key.
type RateLimiter interface {
	Allow(key string, now time.Time) (bool, time.Duration)
}

// ListenPolicySource gives the global listen policy (settings).
type ListenPolicySource interface {
	ListenPolicy(ctx context.Context) string
}

// AuthzRequest is a media upgrade seen by the gateway.
type AuthzRequest struct {
	NodeID    string
	Subject   Subject
	Origin    string
	IP        string
	UserAgent string
}

// Grant is a successful authz: the access token, its connection id and the
// node address to proxy to (host:port).
type Grant struct {
	Token    string
	CID      string
	Upstream string
}

// RateLimitedError carries the wait before a retry.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return domain.ErrTokenRateLimited.Error() }

// Unwrap returns the domain error.
func (e *RateLimitedError) Unwrap() error { return domain.ErrTokenRateLimited }

// MediaAccessOptions are the dependencies of MediaAccess.
type MediaAccessOptions struct {
	Nodes    domain.NodeRepository
	Devices  domain.DeviceRepository
	Tracker  *Tracker
	Presence *Presence
	Issuer   TokenIssuer
	Policy   ListenPolicySource
	// HubURL is the token issuer; its origin is the only accepted Origin.
	HubURL string
	// Upgrades limits media upgrades per client address (10/min, §5.12);
	// Mints limits tokens per session, or address when anonymous
	// (30/min).
	Upgrades RateLimiter
	Mints    RateLimiter
	TTL      time.Duration
	Now      Clock
	Logger   *slog.Logger
}

// MediaAccess is the gateway forward auth of node media connections
// (GRID-011, §5.9, §5.16): it checks the node, the Origin, the listen
// policy and the rate limits, records the connection and mints the access
// token.
type MediaAccess struct {
	o      MediaAccessOptions
	origin string
}

// NewMediaAccess returns the service.
func NewMediaAccess(o MediaAccessOptions) (*MediaAccess, error) {
	origin, err := OriginOf(o.HubURL)
	if err != nil {
		return nil, err
	}

	if o.TTL == 0 {
		o.TTL = AccessTokenTTL
	}

	return &MediaAccess{o: o, origin: origin}, nil
}

// OriginOf returns scheme://host[:port] of a URL.
func OriginOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q", raw)
	}

	return u.Scheme + "://" + u.Host, nil
}

// Authorize answers one media upgrade.
func (a *MediaAccess) Authorize(ctx context.Context, req AuthzRequest) (Grant, error) {
	now := a.o.Now()

	if ok, wait := a.o.Upgrades.Allow("ip:"+req.IP, now); !ok {
		return Grant{}, &RateLimitedError{RetryAfter: wait}
	}

	id, err := domain.NewNodeID(req.NodeID)
	if err != nil {
		return Grant{}, domain.ErrNodeNotFound
	}

	n, err := a.o.Nodes.Get(ctx, id)
	if err != nil {
		return Grant{}, err
	}

	if !n.Active() {
		return Grant{}, domain.ErrNodeNotFound
	}

	if err := a.checkLink(id, n); err != nil {
		return Grant{}, err
	}

	if req.Origin != a.origin {
		return Grant{}, domain.ErrOriginDenied
	}

	devices, err := a.o.Devices.ListByNode(ctx, id)
	if err != nil {
		return Grant{}, fmt.Errorf("list devices of node %s: %w", id, err)
	}

	s := req.Subject
	scopes := a.scopes(ctx, s, devices)

	if len(devices) > 0 && len(scopes) == 0 {
		if s.IsAnonymous() {
			return Grant{}, domain.ErrListenLoginNeeded
		}

		return Grant{}, domain.ErrListenForbidden
	}

	mintKey := "ip:" + req.IP
	if !s.IsAnonymous() {
		mintKey = "session:" + s.SessionID().String()
	}

	if ok, wait := a.o.Mints.Allow(mintKey, now); !ok {
		return Grant{}, &RateLimitedError{RetryAfter: wait}
	}

	cid, err := shared.NewUUIDv7(now)
	if err != nil {
		return Grant{}, fmt.Errorf("connection id: %w", err)
	}

	info := domain.ConnectionInfo{
		ID: cid, Kind: domain.ConnectionMedia, RoleID: s.RoleRank(), IP: domain.NormalizeIP(req.IP),
		UserAgent: truncate(req.UserAgent, 256), NodeID: id.String(),
	}

	claims := token.Claims{
		Issuer: a.o.HubURL, Audience: token.Audience(id.String()), Subject: token.AnonymousSubject,
		ConnectionID: cid.String(), Roles: s.Roles(), Scopes: scopes, Limits: token.Limits{MaxDemods: maxDemods[s.Role()]},
		IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(a.o.TTL), ID: rand.Text(),
	}

	if !s.IsAnonymous() {
		info.UserID, info.SessionID = s.UserID(), s.SessionID()
		claims.Subject = s.UserID().String()
		claims.SessionID = token.SessionRef(s.SessionID().String())
	}

	raw, err := a.o.Issuer.Issue(ctx, claims)
	if err != nil {
		return Grant{}, fmt.Errorf("issue access token: %w", err)
	}

	if err := a.o.Presence.Open(ctx, info); err != nil {
		return Grant{}, fmt.Errorf("record connection: %w", err)
	}

	a.o.Logger.DebugContext(ctx, "media connection authorized",
		slog.String("node_id", id.String()), slog.String("cid", cid.String()), slog.Int("devices", len(scopes)))

	return Grant{Token: raw, CID: cid.String(), Upstream: n.URL().HostPort()}, nil
}

// checkLink answers 503 for a node without an open, compatible channel.
func (a *MediaAccess) checkLink(id domain.NodeID, n *domain.Node) error {
	if a.o.Tracker == nil {
		return domain.ErrNodeOffline
	}

	st, ok := a.o.Tracker.State(id)

	switch {
	case !ok || !st.Connected:
		return domain.ErrNodeOffline
	case st.Compat.Level == domain.CompatIncompatible || n.Runtime().Status == domain.StatusIncompatible:
		return domain.ErrNodeIncompatible
	}

	return nil
}

// scopes computes the scp claim (§5.8, §5.11): listen and demod where the
// effective listen policy allows the subject, preset for operators of the
// device, retune for them when the node allows operators to retune, all
// for admins.
func (a *MediaAccess) scopes(ctx context.Context, s Subject, devices []*domain.Device) []token.Scope {
	global := ListenAnonymous
	if a.o.Policy != nil {
		global = a.o.Policy.ListenPolicy(ctx)
	}

	out := []token.Scope{}

	for _, d := range devices {
		f := d.Flags()
		if !f.Enabled {
			continue
		}

		policy := global
		if f.ListenPolicy != "" {
			policy = f.ListenPolicy
		}

		dev := d.ID().String()
		admin := s.HasOnDevice(RoleAdmin, dev)
		operator := admin || s.HasOnDevice(RoleOperator, dev)

		if !admin && !operator && !canListen(s, policy) {
			continue
		}

		perms := []string{token.PermListen, token.PermDemod}
		if operator {
			perms = append(perms, token.PermPreset)
		}

		if admin || (operator && f.OperatorCanRetune) {
			perms = append(perms, token.PermRetune)
		}

		out = append(out, token.Scope{Device: dev, Perms: perms})
	}

	return out
}

func canListen(s Subject, policy string) bool {
	switch policy {
	case ListenAnonymous:
		return true
	case ListenRegistered:
		return !s.IsAnonymous()
	default:
		return false
	}
}

// IsRateLimited returns the wait of a rate-limited authz.
func IsRateLimited(err error) (time.Duration, bool) {
	var rl *RateLimitedError
	if errors.As(err, &rl) {
		return rl.RetryAfter, true
	}

	return 0, false
}
