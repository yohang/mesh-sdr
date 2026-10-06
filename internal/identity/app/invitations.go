package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Invitations runs the invitations (ACC-002, TECHNICAL_SPEC §5.4): the only
// way to create an account from the web UI.
type Invitations struct {
	invitations domain.InvitationRepository
	users       domain.UserRepository
	audit       domain.AuditLog
	tx          Transactor
	hasher      PasswordHasher
	ids         IDGenerator
	now         Clock
	settings    Settings
	policies    Policies
	auth        *Auth
	notifier    Notifier
	links       Links
	limiter     IPLimiter
	logger      *slog.Logger
}

// InvitationsDeps are the dependencies of Invitations.
type InvitationsDeps struct {
	Invitations domain.InvitationRepository
	Users       domain.UserRepository
	Audit       domain.AuditLog
	Tx          Transactor
	Hasher      PasswordHasher
	IDs         IDGenerator
	Now         Clock
	Settings    Settings
	Policies    Policies
	// Auth opens the session of a new account.
	Auth     *Auth
	Notifier Notifier
	Links    Links
	// Limiter rate-limits checks and acceptances per client address.
	Limiter IPLimiter
	Logger  *slog.Logger
}

// NewInvitations returns the service.
func NewInvitations(d InvitationsDeps) *Invitations {
	return &Invitations{
		invitations: d.Invitations, users: d.Users, audit: d.Audit, tx: d.Tx, hasher: d.Hasher, ids: d.IDs, now: d.Now,
		settings: d.Settings, policies: d.Policies, auth: d.Auth, notifier: d.Notifier, links: d.Links, limiter: d.Limiter,
		logger: d.Logger,
	}
}

// MailEnabled reports whether invitations can be e-mailed.
func (s *Invitations) MailEnabled() bool { return s.notifier != nil && s.notifier.Enabled() }

// DefaultTTL returns the validity of a new invitation (invitations.ttl_hours).
func (s *Invitations) DefaultTTL(ctx context.Context) time.Duration {
	return s.settings.InvitationTTL(ctx)
}

// MinLength returns the minimum password length in force.
func (s *Invitations) MinLength(ctx context.Context) int { return s.policies.Password(ctx).MinLength() }

// CreateInvitationInput is a new invitation.
type CreateInvitationInput struct {
	Role   domain.Role
	Device string // optional, operator only
	Email  string // optional
	// TTL is the validity; zero uses invitations.ttl_hours.
	TTL time.Duration
}

// CreatedInvitation is a new invitation with its link, shown once to the
// admin who created it (SR-08).
type CreatedInvitation struct {
	Invitation *domain.Invitation
	Link       string
	// Mailed tells that the link was also e-mailed.
	Mailed bool
}

// Create creates an invitation. With an e-mail address and mail
// configured, the link is also e-mailed.
func (s *Invitations) Create(ctx context.Context, by Actor, in CreateInvitationInput) (CreatedInvitation, error) {
	var (
		dev   domain.DeviceID
		email domain.Email
		err   error
	)

	if in.Device != "" {
		if dev, err = domain.NewDeviceID(in.Device); err != nil {
			return CreatedInvitation{}, err
		}
	}

	if in.Email != "" {
		if email, err = domain.NewEmail(in.Email); err != nil {
			return CreatedInvitation{}, err
		}
	}

	ttl := in.TTL
	if ttl == 0 {
		ttl = s.DefaultTTL(ctx)
	}

	delivery := domain.DeliveryLink
	if !email.IsZero() && s.MailEnabled() {
		delivery = domain.DeliveryEmail
	}

	now := s.now()

	raw, err := s.ids.New(now)
	if err != nil {
		return CreatedInvitation{}, fmt.Errorf("invitation id: %w", err)
	}

	id, err := domain.NewInvitationID(raw)
	if err != nil {
		return CreatedInvitation{}, err
	}

	inv, tok, err := domain.NewInvitation(domain.NewInvitationParams{
		ID: id, Role: in.Role, Device: dev, Email: email, Delivery: delivery, CreatedBy: by.Principal.UserID(), Now: now, TTL: ttl,
	})
	if err != nil {
		return CreatedInvitation{}, err
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := s.invitations.Add(ctx, inv); err != nil {
			return err
		}

		return s.record(ctx, by.audit(), by.Meta.RequestID, domain.ActionInvitationCreate, inv, map[string]string{
			"role": inv.Role().String(), "device": inv.Device().String(), "delivery": string(inv.Delivery()),
			"expires_at": inv.ExpiresAt().Format(time.RFC3339),
		})
	})
	if err != nil {
		return CreatedInvitation{}, wrap("create invitation", err)
	}

	out := CreatedInvitation{Invitation: inv, Link: s.links.Invitation(tok)}

	if delivery == domain.DeliveryEmail {
		if err := s.notifier.Invitation(ctx, email, out.Link, inv.Role(), inv.ExpiresAt()); err != nil {
			s.logger.WarnContext(ctx, "invitation not e-mailed", slog.String("invitation_id", id.String()), slog.Any("error", err))
		} else {
			out.Mailed = true
		}
	}

	s.logger.InfoContext(ctx, "invitation created", slog.String("invitation_id", id.String()), slog.String("role", inv.Role().String()),
		slog.Bool("mailed", out.Mailed))

	return out, nil
}

// List returns the invitations, newest first.
func (s *Invitations) List(ctx context.Context) ([]*domain.Invitation, error) {
	list, err := s.invitations.List(ctx, 500)

	return list, wrap("list invitations", err)
}

// Now returns the current time, to compute invitation states.
func (s *Invitations) Now() time.Time { return s.now() }

// Revoke revokes a pending invitation.
func (s *Invitations) Revoke(ctx context.Context, by Actor, id domain.InvitationID) error {
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		inv, err := s.invitations.ByID(ctx, id)
		if err != nil {
			return err
		}

		if err := inv.Revoke(s.now()); err != nil {
			return err
		}

		if err := s.invitations.Save(ctx, inv); err != nil {
			return err
		}

		return s.record(ctx, by.audit(), by.Meta.RequestID, domain.ActionInvitationRevoke, inv, nil)
	})

	return wrap("revoke invitation", err)
}

func (s *Invitations) allow(meta RequestMeta) error {
	if s.limiter == nil {
		return nil
	}

	if ok, wait := s.limiter.Allow(meta.IP, s.now()); !ok {
		return domain.NewRateLimitError(wait)
	}

	return nil
}

// Check returns the pending invitation of a link, or ErrInvitationInvalid
// (unknown, expired, revoked or used: one answer). Checks are
// rate-limited per client address.
func (s *Invitations) Check(ctx context.Context, token string, meta RequestMeta) (*domain.Invitation, error) {
	if err := s.allow(meta); err != nil {
		return nil, err
	}

	return s.pending(ctx, token)
}

func (s *Invitations) pending(ctx context.Context, token string) (*domain.Invitation, error) {
	lt, err := domain.ParseLinkToken(token)
	if err != nil {
		return nil, domain.ErrInvitationInvalid
	}

	inv, err := s.invitations.ByTokenHash(ctx, lt.Hash())
	if err != nil {
		return nil, wrap("load invitation", err)
	}

	if inv.StateAt(s.now()) != domain.InvitationPending {
		return nil, domain.ErrInvitationInvalid
	}

	return inv, nil
}

// AcceptInput is the account an invitee creates.
type AcceptInput struct {
	Token       string
	Username    string
	DisplayName string // optional
	// Email is the invitee's address when the invitation has none
	// (optional, unconfirmed). An invitation's own address always wins.
	Email    string
	Password string
	Previous string
	Meta     RequestMeta
}

// Accept creates the account of an invitation with the invitation's role,
// marks the invitation used, and signs the invitee in. The address of an
// e-mailed invitation is stored confirmed. An invitation whose address
// already belongs to an account cannot be used (ErrInvitationInvalid: it
// does not tell that the account exists, SR-06).
func (s *Invitations) Accept(ctx context.Context, in AcceptInput) (LoginResult, error) {
	if err := s.allow(in.Meta); err != nil {
		return LoginResult{}, err
	}

	inv, err := s.pending(ctx, in.Token)
	if err != nil {
		return LoginResult{}, err
	}

	u, err := s.newUser(ctx, inv, in)
	if err != nil {
		return LoginResult{}, err
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		inv, err := s.pending(ctx, in.Token) // again, in the transaction
		if err != nil {
			return err
		}

		if err := s.creatorStillAdmin(ctx, inv); err != nil {
			return err
		}

		if err := s.users.Add(ctx, u); err != nil {
			// Never tell that an address has an account (SR-06): an
			// invited address refuses the invitation, a typed one is
			// "unusable" like a malformed one.
			if errors.Is(err, domain.ErrEmailTaken) && !inv.Email().IsZero() {
				return domain.ErrInvitationInvalid
			}

			if errors.Is(err, domain.ErrEmailTaken) {
				return domain.ErrEmailUnusable
			}

			return err
		}

		if err := inv.Redeem(u.ID(), s.now()); err != nil {
			return err
		}

		if err := s.invitations.Save(ctx, inv); err != nil {
			return err
		}

		actor := domain.AnonymousActor(in.Meta.IP)
		if err := s.record(ctx, actor, in.Meta.RequestID, domain.ActionInvitationRedeem, inv, map[string]string{"user_id": u.ID().String()}); err != nil {
			return err
		}

		e, err := domain.NewAuditEntry(s.now(), actor, domain.ActionUserCreate, domain.ResultOK)
		if err != nil {
			return err
		}

		return s.audit.Append(ctx, e.WithTarget("user", u.ID().String()).WithRequestID(in.Meta.RequestID).
			WithAfter(map[string]string{"role": u.Role().String(), "via": "invitation", "invitation_id": inv.ID().String()}))
	})
	if err != nil {
		return LoginResult{}, wrap("accept invitation", err)
	}

	s.logger.InfoContext(ctx, "invitation accepted", slog.String("invitation_id", inv.ID().String()), slog.String("user_id", u.ID().String()))

	return s.auth.OpenSession(ctx, u.ID(), LoginInput{Previous: in.Previous, Meta: in.Meta})
}

// creatorStillAdmin refuses an invitation whose creator is no longer an
// enabled admin (demoted, disabled or deleted): what it granted was the
// creator's to grant.
func (s *Invitations) creatorStillAdmin(ctx context.Context, inv *domain.Invitation) error {
	if inv.CreatedBy().IsZero() {
		return domain.ErrInvitationInvalid
	}

	u, err := s.users.ByID(ctx, inv.CreatedBy())
	if errors.Is(err, domain.ErrUserNotFound) {
		return domain.ErrInvitationInvalid
	}

	if err != nil {
		return err
	}

	if !u.Enabled() || !u.IsAdmin() {
		return domain.ErrInvitationInvalid
	}

	return nil
}

func (s *Invitations) newUser(ctx context.Context, inv *domain.Invitation, in AcceptInput) (*domain.User, error) {
	name, err := domain.NewUsername(in.Username)
	if err != nil {
		return nil, err
	}

	var display domain.DisplayName
	if in.DisplayName != "" {
		if display, err = domain.NewDisplayName(in.DisplayName); err != nil {
			return nil, err
		}
	}

	email := inv.Email()
	if email.IsZero() && in.Email != "" {
		if email, err = domain.NewEmail(in.Email); err != nil {
			return nil, err
		}
	}

	pw, err := newPassword(in.Password, s.policies.Password(ctx))
	if err != nil {
		return nil, err
	}

	hash, err := s.hasher.Hash(ctx, pw.Reveal())
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	now := s.now()

	raw, err := s.ids.New(now)
	if err != nil {
		return nil, fmt.Errorf("user id: %w", err)
	}

	id, err := domain.NewUserID(raw)
	if err != nil {
		return nil, err
	}

	u, err := domain.NewLocalUser(domain.NewLocalUserParams{
		ID: id, Username: name, Email: email, DisplayName: display, PasswordHash: hash, Grants: inv.Grants(),
		Origin: domain.OriginDB, Now: now,
	})
	if err != nil {
		return nil, err
	}

	// An e-mailed invitation proves the address it was sent to (§5.4); a
	// copied link proves nothing.
	if inv.Delivery() == domain.DeliveryEmail {
		u.SetEmail(email, true, now)
	}

	return u, nil
}

func (s *Invitations) record(ctx context.Context, actor domain.Actor, requestID, action string, inv *domain.Invitation, after map[string]string) error {
	e, err := domain.NewAuditEntry(s.now(), actor, action, domain.ResultOK)
	if err != nil {
		return err
	}

	return s.audit.Append(ctx, e.WithTarget("invitation", inv.ID().String()).WithRequestID(requestID).WithAfter(after))
}

// TestMail sends a test message to the actor's own address (Admin ›
// Invitations, "Send test e-mail").
func (s *Invitations) TestMail(ctx context.Context, by Actor) (domain.Email, error) {
	if !s.MailEnabled() {
		return domain.Email{}, ErrMailDisabled
	}

	u, err := s.users.ByID(ctx, by.Principal.UserID())
	if err != nil {
		return domain.Email{}, wrap("load account", err)
	}

	if u.Email().IsZero() {
		return domain.Email{}, domain.ErrInvalidEmail.WithDetail("your account has no e-mail address")
	}

	return u.Email(), s.notifier.Test(ctx, u.Email())
}
