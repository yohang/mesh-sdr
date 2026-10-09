package domain

import (
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func nonNil(u shared.UUID, err error) (shared.UUID, error) {
	if err != nil || u.IsZero() {
		return shared.UUID{}, ErrInvalidID
	}

	return u, nil
}

// UserID identifies a user (UUIDv7 surrogate key).
type UserID struct{ v shared.UUID }

// NewUserID wraps a UUID (not the nil UUID).
func NewUserID(u shared.UUID) (UserID, error) {
	v, err := nonNil(u, nil)

	return UserID{v: v}, err
}

// UserIDFromBytes returns the user id held in 16 bytes (a BLOB(16) column).
func UserIDFromBytes(b []byte) (UserID, error) {
	v, err := nonNil(shared.UUIDFromBytes(b))

	return UserID{v: v}, err
}

// ParseUserID parses the canonical textual UUID form.
func ParseUserID(s string) (UserID, error) {
	v, err := nonNil(shared.ParseUUID(s))

	return UserID{v: v}, err
}

// String returns the canonical textual form.
func (u UserID) String() string { return u.v.String() }

// Bytes returns the 16 bytes.
func (u UserID) Bytes() []byte { return u.v.Bytes() }

// IsZero reports whether u is the zero value.
func (u UserID) IsZero() bool { return u.v.IsZero() }

// UUID returns the UUID.
func (u UserID) UUID() shared.UUID { return u.v }

// SessionID identifies a session row. It is never sent to clients.
type SessionID struct{ v shared.UUID }

// NewSessionID wraps a UUID (not the nil UUID).
func NewSessionID(u shared.UUID) (SessionID, error) {
	v, err := nonNil(u, nil)

	return SessionID{v: v}, err
}

// SessionIDFromBytes returns the session id held in 16 bytes.
func SessionIDFromBytes(b []byte) (SessionID, error) {
	v, err := nonNil(shared.UUIDFromBytes(b))

	return SessionID{v: v}, err
}

// String returns the canonical textual form.
func (s SessionID) String() string { return s.v.String() }

// Bytes returns the 16 bytes.
func (s SessionID) Bytes() []byte { return s.v.Bytes() }

// IsZero reports whether s is the zero value.
func (s SessionID) IsZero() bool { return s.v.IsZero() }

// UUID returns the UUID.
func (s SessionID) UUID() shared.UUID { return s.v }

// InvitationID identifies an invitation (UUIDv7). Admins see it.
type InvitationID struct{ v shared.UUID }

// NewInvitationID wraps a UUID (not the nil UUID).
func NewInvitationID(u shared.UUID) (InvitationID, error) {
	v, err := nonNil(u, nil)

	return InvitationID{v: v}, err
}

// InvitationIDFromBytes returns the invitation id held in 16 bytes.
func InvitationIDFromBytes(b []byte) (InvitationID, error) {
	v, err := nonNil(shared.UUIDFromBytes(b))

	return InvitationID{v: v}, err
}

// ParseInvitationID parses the canonical textual UUID form.
func ParseInvitationID(s string) (InvitationID, error) {
	v, err := nonNil(shared.ParseUUID(s))

	return InvitationID{v: v}, err
}

// String returns the canonical textual form.
func (i InvitationID) String() string { return i.v.String() }

// Bytes returns the 16 bytes.
func (i InvitationID) Bytes() []byte { return i.v.Bytes() }

// IsZero reports whether i is the zero value.
func (i InvitationID) IsZero() bool { return i.v.IsZero() }

// TokenID identifies a one-time token row (password reset, e-mail change).
// It is never sent to clients.
type TokenID struct{ v shared.UUID }

// NewTokenID wraps a UUID (not the nil UUID).
func NewTokenID(u shared.UUID) (TokenID, error) {
	v, err := nonNil(u, nil)

	return TokenID{v: v}, err
}

// TokenIDFromBytes returns the token id held in 16 bytes.
func TokenIDFromBytes(b []byte) (TokenID, error) {
	v, err := nonNil(shared.UUIDFromBytes(b))

	return TokenID{v: v}, err
}

// Bytes returns the 16 bytes.
func (i TokenID) Bytes() []byte { return i.v.Bytes() }

// IsZero reports whether i is the zero value.
func (i TokenID) IsZero() bool { return i.v.IsZero() }
