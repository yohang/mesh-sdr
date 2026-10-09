package wire

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/http/clientip"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/version"
)

// The hub events WebSocket (ADR 0016, ADR 0018): the /api/ws module, its
// topic authorisation and session adapters. events_grid.go turns grid
// changes into hub events.

// eventsIdentity is the identity surface the events adapters need.
type eventsIdentity interface {
	Authorize(ctx context.Context, role identitydomain.Role) error
	Principal(ctx context.Context) identitydomain.Principal
	SessionRef(ctx context.Context) string
	CheckSession(ctx context.Context, r *http.Request) (time.Time, error)
}

// reloadListen reloads the listen policies when they may have changed (a
// settings change, a device report, a forgotten device); when they did,
// every socket re-authorises its topics against the new view.
func reloadListen(p *gridapp.ListenPolicies, b *events.Broker, logger *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		changed, err := p.Refresh(ctx)
		if err != nil {
			// The view is dropped: the sockets re-authorise against a
			// fresh load, failing closed while it fails.
			logger.ErrorContext(ctx, "reload listen policies", slog.Any("error", err))
		}

		if changed || err != nil {
			b.RecheckAll()
		}
	}
}

// topicAuthz decides topic access (§6.6 "Topic access", ADR 0016 decision
// 7): admin topics, the device logs among them, need admin (and
// admin.allowed_networks); per-device
// topics need listen permission on the device (its effective listen
// policy); the other topics are open to signed-in users, and to anonymous
// visitors when some device is anonymous-listenable (public_map does not
// exist yet).
type topicAuthz struct {
	id       eventsIdentity
	policies *gridapp.ListenPolicies
}

func (a topicAuthz) AuthorizeTopic(ctx context.Context, t events.Topic) error {
	p := a.id.Principal(ctx)

	if t.IsAdmin() {
		if a.id.Authorize(ctx, identitydomain.RoleAdmin) != nil {
			return events.ErrTopicForbidden
		}

		return nil
	}

	if t.Device() == "" && !p.IsAnonymous() {
		return nil
	}

	view, err := a.policies.View(ctx)
	if err != nil {
		return err
	}

	switch {
	case t.Device() != "" && !view.CanListen(p.IsAnonymous(), t.Device()):
		return events.ErrTopicForbidden
	case t.Device() == "" && !view.AnyAnonymous():
		return events.ErrTopicForbidden
	}

	return nil
}

// eventsSession adapts identity to the events WS session port.
type eventsSession struct{ id eventsIdentity }

func (s eventsSession) Identify(ctx context.Context) events.Identity {
	p := s.id.Principal(ctx)
	if p.IsAnonymous() {
		return events.Identity{Roles: []string{}}
	}

	roles := make([]string, 0, 3)
	for _, r := range p.Roles() {
		roles = append(roles, r.String())
	}

	return events.Identity{
		Viewer: events.Viewer{UserID: p.UserID().String(), SessionRef: s.id.SessionRef(ctx), Staff: p.Has(identitydomain.RoleOperator),
			Admin: p.Has(identitydomain.RoleAdmin),
		},
		UserID: p.UserID().UUID(), SessionID: p.SessionID().UUID(), Name: p.Name(), Roles: roles, RoleRank: int(p.Role().ID()),
	}
}

func (s eventsSession) Check(ctx context.Context, r *http.Request) (time.Time, error) {
	until, err := s.id.CheckSession(ctx, r)
	if errors.Is(err, identitydomain.ErrUnauthenticated) {
		return time.Time{}, events.ErrUnauthenticated
	}

	return until, err
}

// eventsPresence records the events sockets in the grid presence registry.
type eventsPresence struct{ p *gridapp.Presence }

func (e eventsPresence) Open(ctx context.Context, c events.Connection) error {
	return e.p.Open(ctx, griddomain.ConnectionInfo{
		ID: c.ID, Kind: griddomain.ConnectionEvents, UserID: c.UserID, SessionID: c.SessionID, RoleID: c.RoleRank,
		IP: c.IP, UserAgent: c.UserAgent,
	})
}

func (e eventsPresence) Heartbeat(ctx context.Context, ids []shared.UUID) error {
	return e.p.Heartbeat(ctx, ids)
}

func (e eventsPresence) Attach(ctx context.Context, id shared.UUID, device string) error {
	return e.p.Attach(ctx, id, device)
}

func (e eventsPresence) Close(ctx context.Context, id shared.UUID, reason events.CloseReason) error {
	return e.p.Close(ctx, id, griddomain.ParseCloseReason(string(reason)))
}

// revocations ends the events sockets of revoked sessions and users (ADR
// 0016 decision 4) and, with the grid, sends the revocations to the nodes
// (sessions by token.SessionRef, users by id), dated with the hub clock.
type revocations struct {
	broker *events.Broker
	nodes  gridapp.RevocationBroadcaster
	now    func() time.Time
}

// PublishRevocation implements identityapp.RevocationPublisher.
func (r revocations) PublishRevocation(ctx context.Context, rv identityapp.Revocation) {
	users := make([]string, 0, len(rv.Users))
	for _, u := range rv.Users {
		users = append(users, u.String())
	}

	r.broker.EndSessions(rv.Sessions, users)

	if r.nodes != nil {
		r.nodes.BroadcastRevocations(ctx, r.now(), rv.Sessions, users)
	}
}

// hubOrigin returns scheme://host of hub.url.
func hubOrigin(hubURL string) string {
	u, err := url.Parse(hubURL)
	if err != nil || u.Host == "" {
		return ""
	}

	return u.Scheme + "://" + u.Host
}

// newEventsModule builds the /api/ws module.
func newEventsModule(hubURL string, b *events.Broker, id eventsIdentity, policies *gridapp.ListenPolicies,
	presence *gridapp.Presence, now func() time.Time, logger *slog.Logger,
) *events.Module {
	return events.New(events.Deps{
		Broker: b, Authz: topicAuthz{id: id, policies: policies}, Session: eventsSession{id: id},
		Presence: eventsPresence{p: presence}, Admission: events.NewAdmission(events.DefaultLimits()),
		ClientIP: func(r *http.Request) string { return clientip.From(r.Context()).String() },
		Origin:   hubOrigin(hubURL), Version: version.String(), Now: now,
		Logger: component(logger, "events.ws"),
	})
}
