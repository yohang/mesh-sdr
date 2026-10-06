package domain

import (
	"context"
	"time"
)

// UserRepository stores User aggregates with their identities and grants.
type UserRepository interface {
	// Add stores a new user. It returns ErrUsernameTaken or ErrEmailTaken.
	Add(ctx context.Context, u *User) error
	// Save stores the user's changed fields when its version matches
	// (ErrVersionConflict otherwise), then calls u.Saved().
	Save(ctx context.Context, u *User) error
	// ByID returns a user or ErrUserNotFound.
	ByID(ctx context.Context, id UserID) (*User, error)
	// ByUsername returns a user (case-insensitive) or ErrUserNotFound.
	ByUsername(ctx context.Context, u Username) (*User, error)
	// ByLogin returns the user whose username or e-mail (case-insensitive)
	// is l, or ErrUserNotFound.
	ByLogin(ctx context.Context, l Login) (*User, error)
	// ByIdentity returns the user owning an identity or ErrUserNotFound.
	ByIdentity(ctx context.Context, i Identity) (*User, error)
	// List returns the enabled users, or every user with includeDisabled,
	// ordered by case-insensitive username.
	List(ctx context.Context, includeDisabled bool) ([]*User, error)
}

// SessionRepository stores sessions.
type SessionRepository interface {
	// Add stores a new session.
	Add(ctx context.Context, s *Session) error
	// Touch stores the activity of a session (last_seen_at and idle expiry),
	// only while it is not revoked: a stale copy never resurrects a session.
	Touch(ctx context.Context, s *Session) error
	// Revoke stores the revocation of a session. An earlier revocation is
	// kept (time and reason).
	Revoke(ctx context.Context, s *Session) error
	// ByTokenHash returns a session (active or not) or ErrSessionNotFound.
	ByTokenHash(ctx context.Context, h TokenHash) (*Session, error)
	// RevokeAllForUser revokes every unrevoked session of a user and
	// returns how many were revoked.
	RevokeAllForUser(ctx context.Context, id UserID, reason RevokeReason, now time.Time) (int, error)
	// DeleteEndedBefore deletes up to limit sessions that expired or were
	// revoked before cutoff, and returns how many were deleted.
	DeleteEndedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error)
}

// AuditLog is the append-only audit repository: it has no update or delete
// operation (retention excepted, owned by the retention job).
type AuditLog interface {
	Append(ctx context.Context, e AuditEntry) error
}
