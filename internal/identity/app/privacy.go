package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// UserEraser removes the personal data of a deleted user that another
// module keeps, in the deletion transaction (SR-64).
type UserEraser interface {
	EraseUser(ctx context.Context, id domain.UserID) error
}

// eraseUser deletes a user and its personal data (ACC-009, SR-64): the
// `users` row with its identities, grants, sessions and tokens (cascade),
// the address of the invitations it redeemed, and what other modules keep.
// The audit log keeps the user id, which identifies nobody once the row is
// gone (pseudonymous id). It runs in the caller's transaction and refuses
// the last enabled admin.
func eraseUser(ctx context.Context, users domain.UserRepository, invitations domain.InvitationRepository, erasers []UserEraser,
	audit domain.AuditLog, entry domain.AuditEntry, id domain.UserID,
) error {
	u, err := users.ByID(ctx, id)
	if err != nil {
		return err
	}

	if u.IsAdmin() && u.Enabled() {
		n, err := users.CountEnabledAdmins(ctx)
		if err != nil {
			return err
		}

		if n <= 1 {
			return domain.ErrLastAdmin
		}
	}

	if invitations != nil {
		if err := invitations.ClearEmailOfUser(ctx, id); err != nil {
			return err
		}
	}

	for _, e := range erasers {
		if err := e.EraseUser(ctx, id); err != nil {
			return err
		}
	}

	if err := users.Delete(ctx, id); err != nil {
		return err
	}

	return audit.Append(ctx, entry.WithTarget("user", id.String()))
}

// Delete deletes a user (admin, ACC-009) and tells nodes.
func (s *Accounts) Delete(ctx context.Context, by Actor, id domain.UserID) error {
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		e, err := domain.NewAuditEntry(s.now(), by.audit(), domain.ActionUserDelete, domain.ResultOK)
		if err != nil {
			return err
		}

		return eraseUser(ctx, s.users, s.invitations, s.erasers, s.audit, e.WithRequestID(by.Meta.RequestID), id)
	})
	if err != nil {
		return wrap("delete user", err)
	}

	s.revocations.PublishRevocation(ctx, Revocation{Users: []domain.UserID{id}})
	s.logger.InfoContext(ctx, "user deleted", slog.String("user_id", id.String()))

	return nil
}

// DeleteOwn deletes the actor's own account after checking its current
// password (SR-04).
func (s *Accounts) DeleteOwn(ctx context.Context, by Actor, currentPassword string) error {
	if by.Principal.IsAnonymous() {
		return domain.ErrUnauthenticated
	}

	if err := s.passwords.CheckCurrent(ctx, by.Principal.UserID(), currentPassword, by.Meta, domain.ActionUserDelete); err != nil {
		return err
	}

	return s.Delete(ctx, by, by.Principal.UserID())
}

// Export is the data of an account (ACC-009, SR-64): the account and its
// active sessions.
type Export struct {
	ExportedAt time.Time       `json:"exported_at"`
	Account    ExportAccount   `json:"account"`
	Sessions   []ExportSession `json:"sessions"`
}

// ExportAccount is the account part of an export.
type ExportAccount struct {
	ID              string     `json:"id"`
	Username        string     `json:"username"`
	DisplayName     string     `json:"display_name,omitempty"`
	Email           string     `json:"email,omitempty"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	Roles           string     `json:"roles"`
	Identities      []string   `json:"identities"`
	Enabled         bool       `json:"enabled"`
	CreatedAt       time.Time  `json:"created_at"`
	LastLoginAt     *time.Time `json:"last_login_at,omitempty"`
}

// ExportSession is a session in an export.
type ExportSession struct {
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	IP         string    `json:"ip,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}

	return &t
}

// ExportUser returns the data of an account (its owner or an admin) and
// audits the export (SR-69).
func (s *Accounts) ExportUser(ctx context.Context, by Actor, id domain.UserID) (Export, error) {
	u, err := s.users.ByID(ctx, id)
	if err != nil {
		return Export{}, wrap("load user", err)
	}

	sessions, err := s.sessions.ActiveForUser(ctx, id, s.now())
	if err != nil {
		return Export{}, wrap("list sessions", err)
	}

	out := Export{
		ExportedAt: s.now().UTC(),
		Account: ExportAccount{
			ID: u.ID().String(), Username: u.Username().String(), DisplayName: u.DisplayName().String(),
			Email: u.Email().String(), EmailVerifiedAt: optTime(u.EmailVerifiedAt()), Roles: GrantNames(u.Grants()),
			Enabled: u.Enabled(), CreatedAt: u.CreatedAt(), LastLoginAt: optTime(u.LastLoginAt()), Identities: []string{},
		},
		Sessions: []ExportSession{},
	}

	for _, i := range u.Identities() {
		out.Account.Identities = append(out.Account.Identities, i.Provider().String())
	}

	for _, x := range sessions {
		es := ExportSession{CreatedAt: x.CreatedAt(), LastSeenAt: x.LastSeenAt(), ExpiresAt: x.AbsoluteExpiresAt(), UserAgent: x.UserAgent()}
		if x.IP().IsValid() {
			es.IP = x.IP().String()
		}

		out.Sessions = append(out.Sessions, es)
	}

	if err := s.record(ctx, by, domain.ActionUserExport, id, nil, nil); err != nil {
		return Export{}, wrap("audit export", err)
	}

	return out, nil
}

// ExportOwn returns the actor's own data.
func (s *Accounts) ExportOwn(ctx context.Context, by Actor) (Export, error) {
	if by.Principal.IsAnonymous() {
		return Export{}, domain.ErrUnauthenticated
	}

	return s.ExportUser(ctx, by, by.Principal.UserID())
}

// Remove deletes a user from the command line (AUTH-009): its sessions go
// at once; the last enabled admin stays.
func (s *UserAdmin) Remove(ctx context.Context, username string) error {
	name, err := domain.NewUsername(username)
	if err != nil {
		return domain.ErrUserNotFound
	}

	var id domain.UserID

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := s.users.ByUsername(ctx, name)
		if err != nil {
			return err
		}

		id = u.ID()

		e, err := domain.NewAuditEntry(s.now(), domain.CLIActor(), domain.ActionUserDelete, domain.ResultOK)
		if err != nil {
			return err
		}

		return eraseUser(ctx, s.users, s.invitations, nil, s.audit, e, id)
	})
	if err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "user removed", slog.String("user_id", id.String()))

	return nil
}
