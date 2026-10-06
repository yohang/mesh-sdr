package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Passwords runs the password change of a signed-in user: forced
// (must_change_password, AUTH-006) or voluntary (AUTH-007).
type Passwords struct {
	users      domain.UserRepository
	sessions   domain.SessionRepository
	audit      domain.AuditLog
	tx         Transactor
	hasher     PasswordHasher
	ids        IDGenerator
	now        Clock
	policies   Policies
	throttle   domain.ThrottlePolicy
	sessionPol domain.SessionPolicy
	logger     *slog.Logger
}

// PasswordsDeps are the dependencies of Passwords.
type PasswordsDeps struct {
	Users         domain.UserRepository
	Sessions      domain.SessionRepository
	Audit         domain.AuditLog
	Tx            Transactor
	Hasher        PasswordHasher
	IDs           IDGenerator
	Now           Clock
	Policies      Policies
	Throttle      domain.ThrottlePolicy
	SessionPolicy domain.SessionPolicy
	Logger        *slog.Logger
}

// NewPasswords returns the service.
func NewPasswords(d PasswordsDeps) *Passwords {
	return &Passwords{
		users: d.Users, sessions: d.Sessions, audit: d.Audit, tx: d.Tx, hasher: d.Hasher, ids: d.IDs, now: d.Now,
		policies: d.Policies, throttle: d.Throttle, sessionPol: d.SessionPolicy, logger: d.Logger,
	}
}

// MinLength returns the minimum password length in force.
func (s *Passwords) MinLength(ctx context.Context) int { return s.policies.Password(ctx).MinLength() }

// ChangePasswordInput is a password change by the signed-in user.
type ChangePasswordInput struct {
	// Session is the session of the request.
	Session *domain.Session
	Current string
	New     string
	Meta    RequestMeta
}

// ChangePasswordResult is the rotated session of the user.
type ChangePasswordResult struct {
	Token     domain.SessionToken
	Session   *domain.Session
	Principal domain.Principal
	// Remember tells that the replaced session was opened with "remember
	// me": the new cookie is persistent too.
	Remember bool
	// Forced tells that the user had to change the password.
	Forced bool
	// RevokedSessions counts the sessions signed out, the replaced one
	// included.
	RevokedSessions int
}

// Change checks the current password, then sets the new one (which follows
// the policy and differs from the current one), clears must_change_password,
// revokes every session of the user and opens a new session that replaces
// the request's one with the same absolute expiry (§5.5 rotation).
//
// A wrong current password counts as a failed login: it is throttled like
// password login (SR-05) and answers a *domain.RateLimitError while the
// account is delayed or locked.
func (s *Passwords) Change(ctx context.Context, in ChangePasswordInput) (ChangePasswordResult, error) {
	if in.Session == nil {
		return ChangePasswordResult{}, domain.ErrUnauthenticated
	}

	if len(in.Current) > MaxPasswordBytes {
		return ChangePasswordResult{}, domain.ErrInvalidCurrentPassword
	}

	pw, err := newPassword(in.New, s.policies.Password(ctx))
	if err != nil {
		return ChangePasswordResult{}, err
	}

	uid := in.Session.UserID()
	now := s.now()

	// Reserve the attempt before verifying the current password, as login
	// does: parallel guesses cannot exceed the throttle thresholds.
	var current domain.PasswordHash

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := s.users.ByID(ctx, uid)
		if err != nil {
			return err
		}

		if blocked, until := u.BlockedAt(now); blocked {
			return domain.NewRateLimitError(until.Sub(now))
		}

		if !u.CanPasswordLogin() {
			return domain.ErrInvalidCurrentPassword
		}

		current = u.PasswordHash()
		u.RecordLoginFailure(now, s.throttle)

		return s.users.Save(ctx, u)
	})
	if err != nil {
		return ChangePasswordResult{}, s.wrap(err)
	}

	ok, err := s.hasher.Verify(ctx, normalizePassword(in.Current), current)
	if err != nil {
		return ChangePasswordResult{}, fmt.Errorf("verify current password: %w", err)
	}

	if !ok {
		s.record(ctx, in.Meta, uid, domain.ResultDenied, map[string]string{"reason": "invalid_current_password"})

		return ChangePasswordResult{}, domain.ErrInvalidCurrentPassword
	}

	same, err := s.hasher.Verify(ctx, pw.Reveal(), current)
	if err != nil {
		return ChangePasswordResult{}, fmt.Errorf("compare new password: %w", err)
	}

	if same {
		s.clearFailures(ctx, uid)

		return ChangePasswordResult{}, domain.PasswordRefused(domain.PasswordSameAsCurrent, "the new password must differ from the current one")
	}

	hash, err := s.hasher.Hash(ctx, pw.Reveal())
	if err != nil {
		return ChangePasswordResult{}, fmt.Errorf("hash password: %w", err)
	}

	res, err := s.apply(ctx, in, current, hash)
	if err != nil {
		return ChangePasswordResult{}, s.wrap(err)
	}

	s.logger.InfoContext(ctx, "password changed", slog.String("user_id", uid.String()), slog.Bool("forced", res.Forced),
		slog.Int("revoked_sessions", res.RevokedSessions))

	return res, nil
}

// apply stores the change. It re-checks, in its transaction, what was
// checked before hashing: the request's session is still active, and the
// stored hash is still the verified one. A concurrent reset, logout or
// password change therefore wins over this change instead of being undone.
func (s *Passwords) apply(ctx context.Context, in ChangePasswordInput, verified, hash domain.PasswordHash) (ChangePasswordResult, error) {
	var res ChangePasswordResult

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.now()

		sess, err := s.sessions.ByTokenHash(ctx, in.Session.TokenHash())
		if errors.Is(err, domain.ErrSessionNotFound) || (err == nil && !sess.ActiveAt(now)) {
			return domain.ErrUnauthenticated
		}

		if err != nil {
			return err
		}

		u, err := s.users.ByID(ctx, in.Session.UserID())
		if errors.Is(err, domain.ErrUserNotFound) {
			return domain.ErrUnauthenticated
		}

		if err != nil {
			return err
		}

		if !u.Enabled() {
			return domain.ErrUnauthenticated
		}

		if u.PasswordHash() != verified {
			return domain.ErrInvalidCurrentPassword
		}

		res.Forced = u.MustChangePassword()

		if err := u.ChangePassword(hash, now); err != nil {
			return err
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		if res.RevokedSessions, err = s.sessions.RevokeAllForUser(ctx, u.ID(), domain.RevokePasswordChange, now); err != nil {
			return err
		}

		sid, err := s.ids.New(now)
		if err != nil {
			return fmt.Errorf("session id: %w", err)
		}

		id, err := domain.NewSessionID(sid)
		if err != nil {
			return err
		}

		res.Remember = in.Session.Persistent(s.sessionPol)

		sess, token, err := domain.StartSession(domain.StartSessionParams{
			ID: id, UserID: u.ID(), Provider: in.Session.Provider(), Remember: res.Remember,
			IP: in.Meta.IP, UserAgent: in.Meta.UserAgent, Policy: s.sessionPol, Now: now,
			NotAfter: in.Session.AbsoluteExpiresAt(),
		})
		if err != nil {
			return err
		}

		if err := s.sessions.Add(ctx, sess); err != nil {
			return err
		}

		e, err := domain.NewAuditEntry(now, domain.UserActor(u.ID(), in.Meta.IP), domain.ActionPasswordChange, domain.ResultOK)
		if err != nil {
			return err
		}

		e = e.WithTarget("user", u.ID().String()).WithRequestID(in.Meta.RequestID).WithAfter(map[string]string{
			"forced": strconv.FormatBool(res.Forced), "revoked_sessions": strconv.Itoa(res.RevokedSessions),
		})

		if err := s.audit.Append(ctx, e); err != nil {
			return err
		}

		res.Token, res.Session, res.Principal = token, sess, domain.UserPrincipal(u, sess)

		return nil
	})

	return res, err
}

// clearFailures resets the throttling counted by the reservation once the
// current password is known to be right. A failure is only logged: the next
// successful check resets it.
func (s *Passwords) clearFailures(ctx context.Context, id domain.UserID) {
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := s.users.ByID(ctx, id)
		if err != nil {
			return err
		}

		u.ClearLoginFailures(s.now())

		return s.users.Save(ctx, u)
	})
	if err != nil {
		s.logger.WarnContext(ctx, "login failures not cleared", slog.String("user_id", id.String()), slog.Any("error", err))
	}
}

func (s *Passwords) record(ctx context.Context, meta RequestMeta, id domain.UserID, result domain.AuditResult, after map[string]string) {
	e, err := domain.NewAuditEntry(s.now(), domain.UserActor(id, meta.IP), domain.ActionPasswordChange, result)
	if err == nil {
		err = s.audit.Append(ctx, e.WithTarget("user", id.String()).WithRequestID(meta.RequestID).WithAfter(after))
	}

	if err != nil {
		s.logger.WarnContext(ctx, "audit entry not written", slog.String("action", domain.ActionPasswordChange), slog.Any("error", err))
	}
}

// wrap keeps domain errors (and rate limits) as they are and adds context to
// the others.
func (s *Passwords) wrap(err error) error {
	var de *shared.Error
	if errors.As(err, &de) {
		return err
	}

	return fmt.Errorf("change password: %w", err)
}
