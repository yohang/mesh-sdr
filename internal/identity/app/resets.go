package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Resets runs password reset by single-use link (ACC-003, TECHNICAL_SPEC
// §5.3, SR-06, SR-07): requested by the user (forgot password) or issued
// by an admin.
type Resets struct {
	tokens      domain.PasswordResetRepository
	users       domain.UserRepository
	sessions    domain.SessionRepository
	audit       domain.AuditLog
	tx          Transactor
	hasher      PasswordHasher
	ids         IDGenerator
	now         Clock
	settings    Settings
	policies    Policies
	notifier    Notifier
	links       Links
	requests    IPLimiter
	accounts    KeyLimiter
	confirms    IPLimiter
	revocations RevocationPublisher
	async       func(func())
	pending     PendingLinks
	logger      *slog.Logger
}

// ResetsDeps are the dependencies of Resets.
type ResetsDeps struct {
	Tokens   domain.PasswordResetRepository
	Users    domain.UserRepository
	Sessions domain.SessionRepository
	Audit    domain.AuditLog
	Tx       Transactor
	Hasher   PasswordHasher
	IDs      IDGenerator
	Now      Clock
	Settings Settings
	Policies Policies
	Notifier Notifier
	Links    Links
	// Requests limits reset requests per client address, Accounts per
	// account (silently), Confirms the link checks and submissions per
	// client address (§5.12).
	Requests    IPLimiter
	Accounts    KeyLimiter
	Confirms    IPLimiter
	Revocations RevocationPublisher
	// Async runs the work of a reset request after the answer, so that the
	// answer takes the same time whether or not the account exists
	// (SR-06). Nil: a new goroutine.
	Async func(func())
	// Pending invalidates the user's other pending links on completion.
	Pending PendingLinks
	Logger  *slog.Logger
}

// NewResets returns the service.
func NewResets(d ResetsDeps) *Resets {
	if d.Async == nil {
		d.Async = func(f func()) { go f() }
	}

	if d.Revocations == nil {
		d.Revocations = NoRevocations{}
	}

	return &Resets{
		tokens: d.Tokens, users: d.Users, sessions: d.Sessions, audit: d.Audit, tx: d.Tx, hasher: d.Hasher, ids: d.IDs,
		now: d.Now, settings: d.Settings, policies: d.Policies, notifier: d.Notifier, links: d.Links, requests: d.Requests,
		accounts: d.Accounts, confirms: d.Confirms, revocations: d.Revocations, async: d.Async, pending: d.Pending, logger: d.Logger,
	}
}

// MailEnabled reports whether users can reset a forgotten password
// themselves.
func (s *Resets) MailEnabled() bool { return s.notifier != nil && s.notifier.Enabled() }

// TTL returns the validity of a reset link (password_reset.ttl_minutes).
func (s *Resets) TTL(ctx context.Context) time.Duration { return s.settings.PasswordResetTTL(ctx) }

// MinLength returns the minimum password length in force.
func (s *Resets) MinLength(ctx context.Context) int { return s.policies.Password(ctx).MinLength() }

func allowIP(l IPLimiter, ip RequestMeta, now time.Time) error {
	if l == nil {
		return nil
	}

	if ok, wait := l.Allow(ip.IP, now); !ok {
		return domain.NewRateLimitError(wait)
	}

	return nil
}

// Request asks for a reset link for login (username or e-mail). It answers
// the same whether or not the account exists (SR-06): the lookup, the
// token and the e-mail happen afterwards. Only a client address that made
// too many requests gets a *domain.RateLimitError. The link goes to the
// account's address, confirmed or not (§5.3, owner decision); an account
// that is disabled, has no local password or no address gets nothing; a
// lock-out never blocks a reset (SR-05).
func (s *Resets) Request(ctx context.Context, login string, meta RequestMeta) error {
	if err := allowIP(s.requests, meta, s.now()); err != nil {
		return err
	}

	if !s.MailEnabled() {
		return nil
	}

	ctx = context.WithoutCancel(ctx)

	s.async(func() { s.issueRequested(ctx, login, meta) })

	return nil
}

func (s *Resets) issueRequested(ctx context.Context, typed string, meta RequestMeta) {
	login, err := domain.NewLogin(typed)
	if err != nil {
		return
	}

	u, err := s.users.ByLogin(ctx, login)
	if errors.Is(err, domain.ErrUserNotFound) {
		s.logger.DebugContext(ctx, "password reset requested for an unknown account")

		return
	}

	if err != nil {
		s.logger.ErrorContext(ctx, "password reset request failed", slog.Any("error", err))

		return
	}

	if !u.Enabled() || !u.HasLocalIdentity() || u.Email().IsZero() {
		s.logger.DebugContext(ctx, "password reset requested for an account that cannot reset", slog.String("user_id", u.ID().String()))

		return
	}

	if s.accounts != nil {
		if ok, _ := s.accounts.Allow(u.ID().String(), s.now()); !ok {
			s.logger.WarnContext(ctx, "password reset requests throttled for an account", slog.String("user_id", u.ID().String()))

			return
		}
	}

	if _, _, err := s.issue(ctx, u, domain.AnonymousActor(meta.IP), meta, true); err != nil {
		s.logger.ErrorContext(ctx, "password reset request failed", slog.String("user_id", u.ID().String()), slog.Any("error", err))
	}
}

// issue stores a token (invalidating the user's earlier ones), audits the
// request and e-mails the link when send is true and mail works. It returns
// the link and whether it was e-mailed.
func (s *Resets) issue(ctx context.Context, u *domain.User, actor domain.Actor, meta RequestMeta, send bool) (string, bool, error) {
	now := s.now()

	raw, err := s.ids.New(now)
	if err != nil {
		return "", false, fmt.Errorf("token id: %w", err)
	}

	id, err := domain.NewTokenID(raw)
	if err != nil {
		return "", false, err
	}

	requestedIP := ""
	if actor.Kind() == domain.ActorAnonymous && meta.IP.IsValid() {
		requestedIP = meta.IP.String()
	}

	tok, link, err := domain.NewPasswordResetToken(id, u.ID(), requestedIP, now, s.TTL(ctx))
	if err != nil {
		return "", false, err
	}

	mail := send && s.MailEnabled() && !u.Email().IsZero()
	if mail {
		tok.MailTo(u.Email())
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := s.tokens.Add(ctx, tok); err != nil {
			return err
		}

		e, err := domain.NewAuditEntry(now, actor, domain.ActionResetRequest, domain.ResultOK)
		if err != nil {
			return err
		}

		return s.audit.Append(ctx, e.WithTarget("user", u.ID().String()).WithRequestID(meta.RequestID).
			WithAfter(map[string]string{"emailed": strconv.FormatBool(mail)}))
	})
	if err != nil {
		return "", false, wrap("issue reset token", err)
	}

	url := s.links.PasswordReset(link)

	if !mail {
		return url, false, nil
	}

	if err := s.notifier.PasswordReset(ctx, u.Email(), url, tok.ExpiresAt()); err != nil {
		return url, false, fmt.Errorf("send reset link: %w", err)
	}

	return url, true, nil
}

// AdminResult is a reset issued by an admin.
type AdminResult struct {
	// Mailed tells that the link went to the user's address.
	Mailed bool
	// Link is the link to copy, shown once, when it was not e-mailed.
	Link string
}

// IssueByAdmin issues a reset link for a user (§5.3 admin-issued reset): it
// is e-mailed when mail works and the user has an address, otherwise
// returned once to the admin.
func (s *Resets) IssueByAdmin(ctx context.Context, by Actor, id domain.UserID) (AdminResult, error) {
	u, err := s.users.ByID(ctx, id)
	if err != nil {
		return AdminResult{}, wrap("load user", err)
	}

	if !u.HasLocalIdentity() {
		return AdminResult{}, domain.ErrInvalidUser.WithDetail("the user has no local password")
	}

	link, mailed, err := s.issue(ctx, u, by.audit(), by.Meta, true)
	if err != nil && link == "" {
		return AdminResult{}, err
	}

	if err != nil {
		s.logger.WarnContext(ctx, "reset link not e-mailed, shown to the admin", slog.Any("error", err))
	}

	if mailed {
		return AdminResult{Mailed: true}, nil
	}

	return AdminResult{Link: link}, nil
}

func (s *Resets) token(ctx context.Context, token string) (*domain.PasswordResetToken, error) {
	lt, err := domain.ParseLinkToken(token)
	if err != nil {
		return nil, domain.ErrInvalidToken
	}

	tok, err := s.tokens.ByTokenHash(ctx, lt.Hash())
	if err != nil {
		return nil, wrap("load reset token", err)
	}

	if !tok.ValidAt(s.now()) {
		return nil, domain.ErrInvalidToken
	}

	return tok, nil
}

// Check reports whether a reset link is valid (unused, unexpired). Checks
// are rate-limited per client address.
func (s *Resets) Check(ctx context.Context, token string, meta RequestMeta) error {
	if err := allowIP(s.confirms, meta, s.now()); err != nil {
		return err
	}

	_, err := s.token(ctx, token)

	return err
}

// Confirm sets the new password of a reset link: in one transaction it
// consumes the token, sets the hash (clearing must_change_password and the
// lock-out, and confirming the address the link reached), revokes every
// session of the user and audits the reset (§7.1, SR-07).
func (s *Resets) Confirm(ctx context.Context, token, password string, meta RequestMeta) error {
	if err := allowIP(s.confirms, meta, s.now()); err != nil {
		return err
	}

	tok, err := s.token(ctx, token)
	if err != nil {
		return err
	}

	pw, err := newPassword(password, s.policies.Password(ctx))
	if err != nil {
		return err
	}

	hash, err := s.hasher.Hash(ctx, pw.Reveal())
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.now()

		if err := tok.Use(now); err != nil {
			return err
		}

		if err := s.tokens.Save(ctx, tok); err != nil {
			return err
		}

		u, err := s.users.ByID(ctx, tok.UserID())
		if err != nil {
			return err
		}

		// A disabled account stays locked out: the link is useless.
		if !u.Enabled() {
			return domain.ErrInvalidToken
		}

		if err := u.CompleteReset(hash, tok.SentTo(), now); err != nil {
			return err
		}

		if err := s.users.Save(ctx, u); err != nil {
			return err
		}

		n, err := s.sessions.RevokeAllForUser(ctx, u.ID(), domain.RevokePasswordReset, now)
		if err != nil {
			return err
		}

		if err := s.pending.invalidate(ctx, u.ID(), now); err != nil {
			return err
		}

		e, err := domain.NewAuditEntry(now, domain.AnonymousActor(meta.IP), domain.ActionResetComplete, domain.ResultOK)
		if err != nil {
			return err
		}

		return s.audit.Append(ctx, e.WithTarget("user", u.ID().String()).WithRequestID(meta.RequestID).
			WithAfter(map[string]string{"revoked_sessions": strconv.Itoa(n)}))
	})
	if err != nil {
		return wrap("reset password", err)
	}

	s.revocations.PublishRevocation(ctx, Revocation{Users: []domain.UserID{tok.UserID()}})
	s.logger.InfoContext(ctx, "password reset", slog.String("user_id", tok.UserID().String()))

	return nil
}
