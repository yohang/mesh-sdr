package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// PasswordChecker checks the current password before a sensitive change
// (SR-04), with the throttling of login.
type PasswordChecker interface {
	CheckCurrent(ctx context.Context, uid domain.UserID, password string, meta RequestMeta, action string) error
}

// Profile runs the account page of a signed-in user (ACC-004): display
// name and e-mail address. The password change is Passwords'.
type Profile struct {
	users     domain.UserRepository
	tokens    domain.EmailChangeRepository
	pending   PendingLinks
	audit     domain.AuditLog
	tx        Transactor
	ids       IDGenerator
	now       Clock
	passwords PasswordChecker
	notifier  Notifier
	links     Links
	logger    *slog.Logger
}

// ProfileDeps are the dependencies of Profile.
type ProfileDeps struct {
	Users  domain.UserRepository
	Tokens domain.EmailChangeRepository
	// Pending invalidates the user's pending links when the address
	// changes (a reset link went to the old one).
	Pending   PendingLinks
	Audit     domain.AuditLog
	Tx        Transactor
	IDs       IDGenerator
	Now       Clock
	Passwords PasswordChecker
	Notifier  Notifier
	Links     Links
	Logger    *slog.Logger
}

// NewProfile returns the service.
func NewProfile(d ProfileDeps) *Profile {
	return &Profile{
		users: d.Users, tokens: d.Tokens, pending: d.Pending, audit: d.Audit, tx: d.Tx, ids: d.IDs, now: d.Now, passwords: d.Passwords,
		notifier: d.Notifier, links: d.Links, logger: d.Logger,
	}
}

// Me returns the actor's account.
func (s *Profile) Me(ctx context.Context, by Actor) (*domain.User, error) {
	if by.Principal.IsAnonymous() {
		return nil, domain.ErrUnauthenticated
	}

	u, err := s.users.ByID(ctx, by.Principal.UserID())
	if errors.Is(err, domain.ErrUserNotFound) {
		return nil, domain.ErrUnauthenticated
	}

	return u, wrap("load account", err)
}

// MailEnabled reports whether e-mail changes are confirmed by a link.
func (s *Profile) MailEnabled() bool { return s.notifier != nil && s.notifier.Enabled() }

// SetDisplayName changes the actor's display name; an empty name removes
// it.
func (s *Profile) SetDisplayName(ctx context.Context, by Actor, name string) (*domain.User, error) {
	var display domain.DisplayName

	if name != "" {
		var err error
		if display, err = domain.NewDisplayName(name); err != nil {
			return nil, err
		}
	}

	var out *domain.User

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := s.users.ByID(ctx, by.Principal.UserID())
		if err != nil {
			return err
		}

		out = u
		before := u.DisplayName().String()

		if !u.SetDisplayName(display, s.now()) {
			return nil
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		return s.record(ctx, by, domain.ActionUserUpdate, u.ID(), map[string]string{"field": "display_name", "set": fmt.Sprint(before != "")},
			map[string]string{"field": "display_name", "set": fmt.Sprint(!display.IsZero())})
	})

	return out, wrap("set display name", err)
}

// EmailChangeResult tells how an e-mail change went.
type EmailChangeResult struct {
	// Pending tells that a confirmation link was sent to the new address:
	// the change applies when it is opened.
	Pending bool
}

// ChangeEmail changes the actor's e-mail address after checking the current
// password (SR-04). With mail, a single-use link confirms the new address
// first; without, the address applies at once, unverified. An empty
// address removes it. The previous address is told.
func (s *Profile) ChangeEmail(ctx context.Context, by Actor, email, currentPassword string) (EmailChangeResult, error) {
	if by.Principal.IsAnonymous() {
		return EmailChangeResult{}, domain.ErrUnauthenticated
	}

	var next domain.Email

	if email != "" {
		var err error
		if next, err = domain.NewEmail(email); err != nil {
			return EmailChangeResult{}, err
		}
	}

	uid := by.Principal.UserID()

	if err := s.passwords.CheckCurrent(ctx, uid, currentPassword, by.Meta, domain.ActionEmailChange); err != nil {
		return EmailChangeResult{}, err
	}

	if !next.IsZero() && s.MailEnabled() {
		return EmailChangeResult{Pending: true}, s.sendConfirmation(ctx, uid, next)
	}

	return EmailChangeResult{}, s.apply(ctx, by.audit(), by.Meta.RequestID, uid, next, false)
}

func (s *Profile) sendConfirmation(ctx context.Context, uid domain.UserID, next domain.Email) error {
	now := s.now()

	raw, err := s.ids.New(now)
	if err != nil {
		return fmt.Errorf("token id: %w", err)
	}

	id, err := domain.NewTokenID(raw)
	if err != nil {
		return err
	}

	tok, link, err := domain.NewEmailChangeToken(id, uid, next, now)
	if err != nil {
		return err
	}

	if err := s.tokens.Add(ctx, tok); err != nil {
		return wrap("store e-mail token", err)
	}

	if err := s.notifier.EmailConfirmation(ctx, next, s.links.EmailConfirmation(link), tok.ExpiresAt()); err != nil {
		return fmt.Errorf("send confirmation: %w", err)
	}

	return nil
}

// CheckEmailToken reports whether a confirmation link is valid (the page
// asks to confirm before anything changes: a GET never changes state).
func (s *Profile) CheckEmailToken(ctx context.Context, token string) error {
	tok, err := s.emailToken(ctx, token)
	if err != nil {
		return err
	}

	if !tok.ValidAt(s.now()) {
		return domain.ErrInvalidToken
	}

	return nil
}

func (s *Profile) emailToken(ctx context.Context, token string) (*domain.EmailChangeToken, error) {
	lt, err := domain.ParseLinkToken(token)
	if err != nil {
		return nil, domain.ErrInvalidToken
	}

	tok, err := s.tokens.ByTokenHash(ctx, lt.Hash())
	if err != nil {
		return nil, wrap("load e-mail token", err)
	}

	return tok, nil
}

// ConfirmEmail applies the address of a confirmation link, verified. The
// link works once; ErrEmailTaken means another account uses the address.
func (s *Profile) ConfirmEmail(ctx context.Context, token string, meta RequestMeta) error {
	tok, err := s.emailToken(ctx, token)
	if err != nil {
		return err
	}

	if err := tok.Use(s.now()); err != nil {
		return err
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := s.tokens.Save(ctx, tok); err != nil {
			return err
		}

		return s.apply(ctx, domain.UserActor(tok.UserID(), meta.IP), meta.RequestID, tok.UserID(), tok.NewEmail(), true)
	})

	return wrap("confirm e-mail", err)
}

// apply sets the address in a transaction, audits it and tells the
// previous address.
func (s *Profile) apply(ctx context.Context, actor domain.Actor, requestID string, uid domain.UserID, next domain.Email, verified bool) error {
	var previous domain.Email

	changed := false

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := s.users.ByID(ctx, uid)
		if err != nil {
			return err
		}

		previous = u.Email()

		// A confirmation link of a disabled account is useless.
		if verified && !u.Enabled() {
			return domain.ErrInvalidToken
		}

		if !u.SetEmail(next, verified, s.now()) {
			return nil
		}

		changed = true

		if err := s.pending.invalidate(ctx, uid, s.now()); err != nil {
			return err
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		e, err := domain.NewAuditEntry(s.now(), actor, domain.ActionEmailChange, domain.ResultOK)
		if err != nil {
			return err
		}

		return s.audit.Append(ctx, e.WithTarget("user", uid.String()).WithRequestID(requestID).
			WithAfter(map[string]string{"verified": fmt.Sprint(verified), "removed": fmt.Sprint(next.IsZero())}))
	})
	if err != nil {
		return wrap("change e-mail", err)
	}

	if changed && !previous.IsZero() && previous.Key() != next.Key() && s.MailEnabled() {
		if err := s.notifier.EmailChanged(ctx, previous, next, s.now()); err != nil {
			s.logger.WarnContext(ctx, "e-mail change notice not sent", slog.Any("error", err))
		}
	}

	if changed {
		s.logger.InfoContext(ctx, "e-mail changed", slog.String("user_id", uid.String()), slog.Bool("verified", verified))
	}

	return nil
}

func (s *Profile) record(ctx context.Context, by Actor, action string, target domain.UserID, before, after map[string]string) error {
	e, err := domain.NewAuditEntry(s.now(), by.audit(), action, domain.ResultOK)
	if err != nil {
		return err
	}

	return s.audit.Append(ctx, e.WithTarget("user", target.String()).WithRequestID(by.Meta.RequestID).WithBefore(before).WithAfter(after))
}
