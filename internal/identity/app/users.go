package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// UserAdmin runs the account administration done from the command line on
// the hub host (AUTH-008, AUTH-012, AUTH-013, AUTH-014).
type UserAdmin struct {
	users    domain.UserRepository
	sessions domain.SessionRepository
	audit    domain.AuditLog
	tx       Transactor
	hasher   PasswordHasher
	ids      IDGenerator
	now      Clock
	policy   Policies
	logger   *slog.Logger
}

// UserAdminDeps are the dependencies of UserAdmin.
type UserAdminDeps struct {
	Users    domain.UserRepository
	Sessions domain.SessionRepository
	Audit    domain.AuditLog
	Tx       Transactor
	Hasher   PasswordHasher
	IDs      IDGenerator
	Now      Clock
	Policy   Policies
	Logger   *slog.Logger
}

// NewUserAdmin returns the service.
func NewUserAdmin(d UserAdminDeps) *UserAdmin {
	return &UserAdmin{
		users: d.Users, sessions: d.Sessions, audit: d.Audit, tx: d.Tx, hasher: d.Hasher, ids: d.IDs,
		now: d.Now, policy: d.Policy, logger: d.Logger,
	}
}

// AddUserInput describes a new account.
type AddUserInput struct {
	Username    string
	Email       string // optional
	DisplayName string // optional
	Role        domain.Role
	// Password is the chosen password. When empty, a random password is
	// generated, returned once and must be changed at the first login.
	Password string
}

// AddUserResult is the created account.
type AddUserResult struct {
	User              *domain.User
	GeneratedPassword string
}

// Add creates an account with a local identity and its role.
func (s *UserAdmin) Add(ctx context.Context, in AddUserInput) (AddUserResult, error) {
	name, err := domain.NewUsername(in.Username)
	if err != nil {
		return AddUserResult{}, err
	}

	var email domain.Email
	if in.Email != "" {
		if email, err = domain.NewEmail(in.Email); err != nil {
			return AddUserResult{}, err
		}
	}

	var display domain.DisplayName
	if in.DisplayName != "" {
		if display, err = domain.NewDisplayName(in.DisplayName); err != nil {
			return AddUserResult{}, err
		}
	}

	var grants []domain.RoleGrant

	switch in.Role {
	case domain.RoleListener, domain.RoleAnonymous:
	case domain.RoleOperator, domain.RoleAdmin:
		g, err := domain.NewRoleGrant(in.Role, domain.DeviceID{})
		if err != nil {
			return AddUserResult{}, err
		}

		grants = append(grants, g)
	default:
		return AddUserResult{}, domain.ErrInvalidRole
	}

	generated := ""
	plain := in.Password

	if plain == "" {
		generated = GeneratePassword()
		plain = generated
	}

	pw, err := newPassword(plain, s.policy.Password(ctx))
	if err != nil {
		return AddUserResult{}, err
	}

	hash, err := s.hasher.Hash(ctx, pw.Reveal())
	if err != nil {
		return AddUserResult{}, fmt.Errorf("hash password: %w", err)
	}

	now := s.now()

	uid, err := s.ids.New(now)
	if err != nil {
		return AddUserResult{}, fmt.Errorf("user id: %w", err)
	}

	id, err := domain.NewUserID(uid)
	if err != nil {
		return AddUserResult{}, err
	}

	u, err := domain.NewLocalUser(domain.NewLocalUserParams{
		ID: id, Username: name, Email: email, DisplayName: display, PasswordHash: hash,
		MustChangePassword: generated != "", Grants: grants, Origin: domain.OriginDB, Now: now,
	})
	if err != nil {
		return AddUserResult{}, err
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := s.users.Add(ctx, u); err != nil {
			return err
		}

		return s.appendAudit(ctx, domain.ActionUserCreate, u, nil, map[string]string{
			"username": u.Username().String(), "role": u.Role().String(),
			"must_change_password": strconv.FormatBool(u.MustChangePassword()),
		})
	})
	if err != nil {
		return AddUserResult{}, fmt.Errorf("add user %s: %w", name, err)
	}

	s.logger.InfoContext(ctx, "user created", slog.String("user_id", id.String()), slog.String("role", u.Role().String()))

	return AddUserResult{User: u, GeneratedPassword: generated}, nil
}

// ResetPasswordResult tells what ResetPassword changed.
type ResetPasswordResult struct {
	User              *domain.User
	GeneratedPassword string
	RevokedSessions   int
}

// ResetPassword sets a new password for a user and revokes all its sessions
// (AUTH-010), with the rules of Add: an empty password generates one,
// returned once, that must be changed at the next sign-in. It clears the
// login lock-out.
func (s *UserAdmin) ResetPassword(ctx context.Context, username, password string) (ResetPasswordResult, error) {
	generated := ""
	if password == "" {
		generated = GeneratePassword()
		password = generated
	}

	pw, err := newPassword(password, s.policy.Password(ctx))
	if err != nil {
		return ResetPasswordResult{}, err
	}

	hash, err := s.hasher.Hash(ctx, pw.Reveal())
	if err != nil {
		return ResetPasswordResult{}, fmt.Errorf("hash password: %w", err)
	}

	res := ResetPasswordResult{GeneratedPassword: generated}

	err = s.withUser(ctx, username, func(ctx context.Context, u *domain.User) error {
		now := s.now()

		if err := u.ResetPassword(hash, generated != "", now); err != nil {
			return err
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		n, err := s.sessions.RevokeAllForUser(ctx, u.ID(), domain.RevokePasswordReset, now)
		if err != nil {
			return err
		}

		res.User, res.RevokedSessions = u, n

		return s.appendAudit(ctx, domain.ActionUserPasswordReset, u, nil, map[string]string{
			"must_change_password": strconv.FormatBool(u.MustChangePassword()), "revoked_sessions": strconv.Itoa(n),
		})
	})
	if err != nil {
		return ResetPasswordResult{}, err
	}

	s.logger.InfoContext(ctx, "password reset", slog.String("user_id", res.User.ID().String()), slog.Int("revoked_sessions", res.RevokedSessions))

	return res, nil
}

// GeneratePassword returns a random password (26 characters from the
// CSPRNG, about 130 bits).
func GeneratePassword() string { return rand.Text() }

// Exists reports whether a user with that username exists.
func (s *UserAdmin) Exists(ctx context.Context, username string) (bool, error) {
	name, err := domain.NewUsername(username)
	if err != nil {
		return false, nil // an invalid name matches no user
	}

	_, err = s.users.ByUsername(ctx, name)

	switch {
	case errors.Is(err, domain.ErrUserNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("load user %s: %w", name, err)
	}

	return true, nil
}

// List returns the enabled users, or every user with includeDisabled, by
// username (AUTH-011).
func (s *UserAdmin) List(ctx context.Context, includeDisabled bool) ([]*domain.User, error) {
	users, err := s.users.List(ctx, includeDisabled)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	return users, nil
}

// DisableResult tells what Disable changed.
type DisableResult struct {
	Changed         bool
	RevokedSessions int
}

// Disable disables an account and revokes all its sessions in the same
// transaction (AUTH-012). Disabling a disabled account changes nothing.
func (s *UserAdmin) Disable(ctx context.Context, username string) (DisableResult, error) {
	var res DisableResult

	err := s.withUser(ctx, username, func(ctx context.Context, u *domain.User) error {
		now := s.now()
		if !u.Disable(now) {
			return nil
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		n, err := s.sessions.RevokeAllForUser(ctx, u.ID(), domain.RevokeUserDisabled, now)
		if err != nil {
			return err
		}

		res = DisableResult{Changed: true, RevokedSessions: n}

		return s.appendAudit(ctx, domain.ActionUserDisable, u, map[string]string{"enabled": "true"},
			map[string]string{"enabled": "false", "revoked_sessions": strconv.Itoa(n)})
	})

	return res, err
}

// Enable enables an account (AUTH-013) and clears its login lock-out.
// Enabling an enabled account changes nothing.
func (s *UserAdmin) Enable(ctx context.Context, username string) (bool, error) {
	changed := false

	err := s.withUser(ctx, username, func(ctx context.Context, u *domain.User) error {
		if !u.Enable(s.now()) {
			return nil
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		changed = true

		return s.appendAudit(ctx, domain.ActionUserEnable, u, map[string]string{"enabled": "false"}, map[string]string{"enabled": "true"})
	})

	return changed, err
}

func (s *UserAdmin) withUser(ctx context.Context, username string, fn func(ctx context.Context, u *domain.User) error) error {
	name, err := domain.NewUsername(username)
	if err != nil {
		return domain.ErrUserNotFound
	}

	return s.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := s.users.ByUsername(ctx, name)
		if err != nil {
			return err
		}

		return fn(ctx, u)
	})
}

func (s *UserAdmin) appendAudit(ctx context.Context, action string, u *domain.User, before, after map[string]string) error {
	e, err := domain.NewAuditEntry(s.now(), domain.CLIActor(), action, domain.ResultOK)
	if err != nil {
		return err
	}

	return s.audit.Append(ctx, e.WithTarget("user", u.ID().String()).WithBefore(before).WithAfter(after))
}
