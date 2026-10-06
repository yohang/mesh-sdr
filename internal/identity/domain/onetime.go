package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"time"
)

// LinkToken is the secret of a single-use link (invitation, password reset,
// e-mail change): 256 random bits, sent once in the link, base64url. Only
// its SHA-256 is stored.
type LinkToken struct{ v [32]byte }

// NewLinkToken returns a random token.
func NewLinkToken() LinkToken {
	var t LinkToken

	_, _ = rand.Read(t.v[:])

	return t
}

// ParseLinkToken decodes the textual form of a link token.
func ParseLinkToken(s string) (LinkToken, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return LinkToken{}, ErrInvalidToken
	}

	var t LinkToken

	copy(t.v[:], b)

	return t, nil
}

// Text returns the textual form, to put in the link.
func (t LinkToken) Text() string { return base64.RawURLEncoding.EncodeToString(t.v[:]) }

// String redacts the token.
func (t LinkToken) String() string { return "<redacted>" }

// Hash returns the stored SHA-256 of the token.
func (t LinkToken) Hash() TokenHash { return TokenHash{v: sha256.Sum256(t.v[:])} }

// Maximum lifetimes of single-use links.
const (
	MaxInvitationTTL    = 30 * 24 * time.Hour
	MaxPasswordResetTTL = 24 * time.Hour
	EmailChangeTTL      = 24 * time.Hour
)

// oneTime is the state shared by single-use tokens bound to a user.
type oneTime struct {
	id        TokenID
	userID    UserID
	tokenHash TokenHash
	createdAt time.Time
	expiresAt time.Time
	usedAt    time.Time
}

func newOneTime(id TokenID, user UserID, now time.Time, ttl time.Duration) (oneTime, LinkToken, error) {
	if id.IsZero() || user.IsZero() || now.IsZero() || ttl <= 0 {
		return oneTime{}, LinkToken{}, ErrInvalidToken
	}

	now = now.UTC().Truncate(time.Millisecond)
	tok := NewLinkToken()

	return oneTime{id: id, userID: user, tokenHash: tok.Hash(), createdAt: now, expiresAt: now.Add(ttl)}, tok, nil
}

// ID returns the token row id.
func (o *oneTime) ID() TokenID { return o.id }

// UserID returns the user the token is bound to.
func (o *oneTime) UserID() UserID { return o.userID }

// TokenHash returns the stored hash.
func (o *oneTime) TokenHash() TokenHash { return o.tokenHash }

// CreatedAt returns the issue time.
func (o *oneTime) CreatedAt() time.Time { return o.createdAt }

// ExpiresAt returns the expiry.
func (o *oneTime) ExpiresAt() time.Time { return o.expiresAt }

// UsedAt returns when the token was used (zero: unused).
func (o *oneTime) UsedAt() time.Time { return o.usedAt }

// ValidAt reports whether the token is unused and unexpired at now.
func (o *oneTime) ValidAt(now time.Time) bool {
	return o.usedAt.IsZero() && now.Before(o.expiresAt)
}

// Use consumes the token; it returns ErrInvalidToken when it is used or
// expired.
func (o *oneTime) Use(now time.Time) error {
	if !o.ValidAt(now) {
		return ErrInvalidToken
	}

	o.usedAt = now.UTC().Truncate(time.Millisecond)

	return nil
}

// OneTimeState is the persisted state of a single-use token.
type OneTimeState struct {
	ID        TokenID
	UserID    UserID
	TokenHash TokenHash
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time
}

func rehydrateOneTime(s OneTimeState) (oneTime, error) {
	if s.ID.IsZero() || s.UserID.IsZero() || s.CreatedAt.IsZero() || s.ExpiresAt.IsZero() {
		return oneTime{}, ErrInvalidToken
	}

	return oneTime{id: s.ID, userID: s.UserID, tokenHash: s.TokenHash, createdAt: s.CreatedAt, expiresAt: s.ExpiresAt, usedAt: s.UsedAt}, nil
}

// PasswordResetToken is a single-use password reset link (ACC-003, §7.1
// `password_reset_tokens`).
type PasswordResetToken struct {
	oneTime

	requestedIP string
}

// NewPasswordResetToken issues a reset token for a user, valid for ttl (at
// most MaxPasswordResetTTL).
func NewPasswordResetToken(id TokenID, user UserID, requestedIP string, now time.Time, ttl time.Duration) (*PasswordResetToken, LinkToken, error) {
	if ttl > MaxPasswordResetTTL {
		return nil, LinkToken{}, ErrInvalidToken.WithDetail("the reset link lifetime is too long")
	}

	o, tok, err := newOneTime(id, user, now, ttl)
	if err != nil {
		return nil, LinkToken{}, err
	}

	return &PasswordResetToken{oneTime: o, requestedIP: truncateUTF8(requestedIP, 45)}, tok, nil
}

// RehydratePasswordResetToken rebuilds a stored reset token.
func RehydratePasswordResetToken(s OneTimeState, requestedIP string) (*PasswordResetToken, error) {
	o, err := rehydrateOneTime(s)
	if err != nil {
		return nil, err
	}

	return &PasswordResetToken{oneTime: o, requestedIP: requestedIP}, nil
}

// RequestedIP returns the address that requested the reset ("" for an
// admin-issued link).
func (t *PasswordResetToken) RequestedIP() string { return t.requestedIP }

// EmailChangeToken confirms a new e-mail address (ACC-004): the change
// applies when its owner opens the link sent to it.
type EmailChangeToken struct {
	oneTime

	newEmail Email
}

// NewEmailChangeToken issues a confirmation token for a new address.
func NewEmailChangeToken(id TokenID, user UserID, newEmail Email, now time.Time) (*EmailChangeToken, LinkToken, error) {
	if newEmail.IsZero() {
		return nil, LinkToken{}, ErrInvalidEmail
	}

	o, tok, err := newOneTime(id, user, now, EmailChangeTTL)
	if err != nil {
		return nil, LinkToken{}, err
	}

	return &EmailChangeToken{oneTime: o, newEmail: newEmail}, tok, nil
}

// RehydrateEmailChangeToken rebuilds a stored e-mail change token.
func RehydrateEmailChangeToken(s OneTimeState, newEmail Email) (*EmailChangeToken, error) {
	if newEmail.IsZero() {
		return nil, ErrInvalidEmail
	}

	o, err := rehydrateOneTime(s)
	if err != nil {
		return nil, err
	}

	return &EmailChangeToken{oneTime: o, newEmail: newEmail}, nil
}

// NewEmail returns the address to confirm.
func (t *EmailChangeToken) NewEmail() Email { return t.newEmail }
