package domain

import (
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Delivery tells how an invitation link reaches the invitee
// (`invitations.delivery`).
type Delivery string

// Deliveries.
const (
	DeliveryEmail Delivery = "email"
	DeliveryLink  Delivery = "link"
)

// InvitationState is the life-cycle state of an invitation.
type InvitationState string

// Invitation states.
const (
	InvitationPending  InvitationState = "pending"
	InvitationRedeemed InvitationState = "redeemed"
	InvitationRevoked  InvitationState = "revoked"
	InvitationExpired  InvitationState = "expired"
)

// Invitation is a single-use invitation to create an account with a pre-set
// role (ACC-002, TECHNICAL_SPEC §5.4, §7.1 `invitations`). The role always
// comes from the invitation, never from the acceptance request (SR-08).
type Invitation struct {
	id             InvitationID
	tokenHash      TokenHash
	delivery       Delivery
	email          Email
	role           Role
	device         shared.DeviceID
	createdBy      UserID
	createdAt      time.Time
	expiresAt      time.Time
	redeemedAt     time.Time
	redeemedUserID UserID
	revokedAt      time.Time
}

// NewInvitationParams is the input of NewInvitation.
type NewInvitationParams struct {
	ID        InvitationID
	Role      Role
	Device    shared.DeviceID // optional, operator only
	Email     Email           // optional
	Delivery  Delivery
	CreatedBy UserID
	Now       time.Time
	TTL       time.Duration
}

// NewInvitation creates a pending invitation and returns its secret token,
// to be shown or sent once.
func NewInvitation(p NewInvitationParams) (*Invitation, LinkToken, error) {
	switch {
	case p.ID.IsZero(), p.Now.IsZero():
		return nil, LinkToken{}, ErrInvalidInvitation
	case p.TTL <= 0 || p.TTL > MaxInvitationTTL:
		return nil, LinkToken{}, ErrInvalidInvitation.WithDetail("an invitation expires after 1 hour to 30 days")
	case p.Delivery != DeliveryEmail && p.Delivery != DeliveryLink:
		return nil, LinkToken{}, ErrInvalidInvitation.WithDetail("invalid delivery")
	case p.Delivery == DeliveryEmail && p.Email.IsZero():
		return nil, LinkToken{}, ErrInvalidInvitation.WithDetail("an e-mailed invitation needs an e-mail address")
	}

	switch p.Role {
	case RoleListener, RoleAdmin:
		if !p.Device.IsZero() {
			return nil, LinkToken{}, ErrInvalidInvitation.WithDetail("only the operator role takes a device scope")
		}
	case RoleOperator:
	default:
		return nil, LinkToken{}, ErrInvalidRole
	}

	now := p.Now.UTC().Truncate(time.Millisecond)
	tok := NewLinkToken()

	return &Invitation{
		id: p.ID, tokenHash: tok.Hash(), delivery: p.Delivery, email: p.Email, role: p.Role, device: p.Device,
		createdBy: p.CreatedBy, createdAt: now, expiresAt: now.Add(p.TTL),
	}, tok, nil
}

// InvitationStateData is the persisted state of an invitation.
type InvitationStateData struct {
	ID             InvitationID
	TokenHash      TokenHash
	Delivery       Delivery
	Email          Email
	Role           Role
	Device         shared.DeviceID
	CreatedBy      UserID
	CreatedAt      time.Time
	ExpiresAt      time.Time
	RedeemedAt     time.Time
	RedeemedUserID UserID
	RevokedAt      time.Time
}

// RehydrateInvitation rebuilds a stored invitation.
func RehydrateInvitation(s InvitationStateData) (*Invitation, error) {
	if s.ID.IsZero() || s.CreatedAt.IsZero() || s.ExpiresAt.IsZero() ||
		(s.Delivery != DeliveryEmail && s.Delivery != DeliveryLink) || s.Role < RoleListener || s.Role > RoleAdmin {
		return nil, ErrInvalidInvitation
	}

	return &Invitation{
		id: s.ID, tokenHash: s.TokenHash, delivery: s.Delivery, email: s.Email, role: s.Role, device: s.Device,
		createdBy: s.CreatedBy, createdAt: s.CreatedAt, expiresAt: s.ExpiresAt, redeemedAt: s.RedeemedAt,
		redeemedUserID: s.RedeemedUserID, revokedAt: s.RevokedAt,
	}, nil
}

// ID returns the invitation id.
func (i *Invitation) ID() InvitationID { return i.id }

// TokenHash returns the stored hash of the token.
func (i *Invitation) TokenHash() TokenHash { return i.tokenHash }

// Delivery returns how the link was delivered.
func (i *Invitation) Delivery() Delivery { return i.delivery }

// Email returns the invited address (zero when none).
func (i *Invitation) Email() Email { return i.email }

// Role returns the pre-set role.
func (i *Invitation) Role() Role { return i.role }

// Device returns the device scope of the role (zero: global).
func (i *Invitation) Device() shared.DeviceID { return i.device }

// CreatedBy returns the admin who created it (zero when deleted).
func (i *Invitation) CreatedBy() UserID { return i.createdBy }

// CreatedAt returns the creation time.
func (i *Invitation) CreatedAt() time.Time { return i.createdAt }

// ExpiresAt returns the expiry.
func (i *Invitation) ExpiresAt() time.Time { return i.expiresAt }

// RedeemedAt returns when it was accepted (zero: not accepted).
func (i *Invitation) RedeemedAt() time.Time { return i.redeemedAt }

// RedeemedUserID returns the account it created (zero when none or deleted).
func (i *Invitation) RedeemedUserID() UserID { return i.redeemedUserID }

// RevokedAt returns when it was revoked (zero: not revoked).
func (i *Invitation) RevokedAt() time.Time { return i.revokedAt }

// StateAt returns the state at now.
func (i *Invitation) StateAt(now time.Time) InvitationState {
	switch {
	case !i.redeemedAt.IsZero():
		return InvitationRedeemed
	case !i.revokedAt.IsZero():
		return InvitationRevoked
	case !now.Before(i.expiresAt):
		return InvitationExpired
	}

	return InvitationPending
}

// Grants returns the role grants of the account it creates: none for a
// listener (implicit).
func (i *Invitation) Grants() []RoleGrant {
	if i.role == RoleListener {
		return nil
	}

	g, err := NewRoleGrant(i.role, i.device)
	if err != nil {
		return nil
	}

	return []RoleGrant{g}
}

// Redeem marks the invitation used by the account it created. It returns
// ErrInvitationInvalid unless it is pending.
func (i *Invitation) Redeem(user UserID, now time.Time) error {
	if i.StateAt(now) != InvitationPending || user.IsZero() {
		return ErrInvitationInvalid
	}

	i.redeemedAt = now.UTC().Truncate(time.Millisecond)
	i.redeemedUserID = user

	return nil
}

// Revoke revokes a pending invitation. It returns ErrInvitationNotPending
// when it was redeemed, revoked or has expired.
func (i *Invitation) Revoke(now time.Time) error {
	if i.StateAt(now) != InvitationPending {
		return ErrInvitationNotPending
	}

	i.revokedAt = now.UTC().Truncate(time.Millisecond)

	return nil
}
