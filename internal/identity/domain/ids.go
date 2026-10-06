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
