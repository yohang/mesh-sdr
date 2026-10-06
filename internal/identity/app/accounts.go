package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Actor is who runs an account operation: a signed-in user through a
// request.
type Actor struct {
	Principal domain.Principal
	Meta      RequestMeta
}

func (a Actor) audit() domain.Actor { return domain.UserActor(a.Principal.UserID(), a.Meta.IP) }

// Revocation tells nodes to close the media sockets of users or sessions
// (TECHNICAL_SPEC §4.4 `ctl.revocations`).
type Revocation struct {
	Users []domain.UserID
	// Sessions are session handles (Session.Ref, the `sid` claim).
	Sessions []string
}

// RevocationPublisher pushes revocations to nodes. It is called after the
// revocation is committed; failures are the publisher's to log.
type RevocationPublisher interface {
	PublishRevocation(ctx context.Context, r Revocation)
}

// NoRevocations publishes nothing: nodes learn of revocations when the
// access tokens of ACC-007 expire.
type NoRevocations struct{}

// PublishRevocation implements RevocationPublisher.
func (NoRevocations) PublishRevocation(context.Context, Revocation) {}

// Accounts runs the account administration of the web UI and API: roles
// (ACC-006), and the user management of ACC-008.
type Accounts struct {
	users       domain.UserRepository
	sessions    domain.SessionRepository
	audit       domain.AuditLog
	tx          Transactor
	now         Clock
	revocations RevocationPublisher
	logger      *slog.Logger
}

// AccountsDeps are the dependencies of Accounts.
type AccountsDeps struct {
	Users       domain.UserRepository
	Sessions    domain.SessionRepository
	Audit       domain.AuditLog
	Tx          Transactor
	Now         Clock
	Revocations RevocationPublisher
	Logger      *slog.Logger
}

// NewAccounts returns the service.
func NewAccounts(d AccountsDeps) *Accounts {
	if d.Revocations == nil {
		d.Revocations = NoRevocations{}
	}

	return &Accounts{
		users: d.Users, sessions: d.Sessions, audit: d.Audit, tx: d.Tx, now: d.Now, revocations: d.Revocations,
		logger: d.Logger,
	}
}

// User returns a user.
func (s *Accounts) User(ctx context.Context, id domain.UserID) (*domain.User, error) {
	u, err := s.users.ByID(ctx, id)
	if err != nil && !errors.Is(err, domain.ErrUserNotFound) {
		return nil, fmt.Errorf("load user %s: %w", id, err)
	}

	return u, err
}

// GrantNames describes grants for audit entries and lists: "operator",
// "operator@rtl-1", "admin", sorted; "listener" when none.
func GrantNames(grants []domain.RoleGrant) string {
	if len(grants) == 0 {
		return domain.RoleListener.String()
	}

	names := make([]string, 0, len(grants))

	for _, g := range grants {
		n := g.Role().String()
		if !g.Global() {
			n += "@" + g.Device().String()
		}

		names = append(names, n)
	}

	slices.Sort(names)

	return strings.Join(names, ",")
}

// RolesResult tells what SetRoles changed.
type RolesResult struct {
	User            *domain.User
	Changed         bool
	RevokedSessions int
}

// SetRoles replaces the role grants of a user (ACC-006). Any change is a
// privilege change: every session of the user is revoked (SR-01, owner
// decision) and nodes are told. The last enabled admin cannot lose the
// admin role (ErrLastAdmin).
func (s *Accounts) SetRoles(ctx context.Context, by Actor, id domain.UserID, grants []domain.RoleGrant) (RolesResult, error) {
	var res RolesResult

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.now()

		u, err := s.users.ByID(ctx, id)
		if err != nil {
			return err
		}

		before := GrantNames(u.Grants())
		wasAdmin := u.IsAdmin()

		if !u.ReplaceGrants(grants, by.Principal.UserID(), now) {
			res.User = u

			return nil
		}

		if wasAdmin && !u.IsAdmin() && u.Enabled() {
			if err := s.keepAnAdmin(ctx); err != nil {
				return err
			}
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		n, err := s.sessions.RevokeAllForUser(ctx, u.ID(), domain.RevokeAdmin, now)
		if err != nil {
			return err
		}

		res = RolesResult{User: u, Changed: true, RevokedSessions: n}

		return s.record(ctx, by, domain.ActionUserRoleUpdate, u.ID(), map[string]string{"roles": before},
			map[string]string{"roles": GrantNames(u.Grants()), "revoked_sessions": strconv.Itoa(n)})
	})
	if err != nil {
		return RolesResult{}, wrap("set roles", err)
	}

	if res.Changed {
		s.revocations.PublishRevocation(ctx, Revocation{Users: []domain.UserID{id}})
		s.logger.InfoContext(ctx, "roles changed", slog.String("user_id", id.String()),
			slog.String("roles", GrantNames(res.User.Grants())), slog.Int("revoked_sessions", res.RevokedSessions))
	}

	return res, nil
}

// keepAnAdmin refuses a change that leaves no enabled admin: it runs in the
// write transaction, before the change, so the count includes the user
// about to lose the role.
func (s *Accounts) keepAnAdmin(ctx context.Context) error {
	n, err := s.users.CountEnabledAdmins(ctx)
	if err != nil {
		return err
	}

	if n <= 1 {
		return domain.ErrLastAdmin
	}

	return nil
}

func (s *Accounts) record(ctx context.Context, by Actor, action string, target domain.UserID, before, after map[string]string) error {
	e, err := domain.NewAuditEntry(s.now(), by.audit(), action, domain.ResultOK)
	if err != nil {
		return err
	}

	return s.audit.Append(ctx, e.WithTarget("user", target.String()).WithRequestID(by.Meta.RequestID).WithBefore(before).WithAfter(after))
}

// wrap keeps domain errors as they are and adds context to the others.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}

	var de *shared.Error
	if errors.As(err, &de) {
		return err
	}

	return fmt.Errorf("%s: %w", op, err)
}
