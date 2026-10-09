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
	// CountEnabledAdmins returns the number of enabled users holding the
	// global admin role.
	CountEnabledAdmins(ctx context.Context) (int, error)
	// Search returns a page of users matching q, by case-insensitive
	// username, after q.After.
	Search(ctx context.Context, q UserQuery) ([]*User, error)
	// Delete deletes a user with its identities, grants, sessions and
	// tokens (ErrUserNotFound when absent).
	Delete(ctx context.Context, id UserID) error
	// Usernames returns the usernames of the users that exist among ids.
	Usernames(ctx context.Context, ids []UserID) (map[UserID]Username, error)
}

// UserQuery filters the user list of the admin (ACC-008).
type UserQuery struct {
	// Text matches the username, e-mail or display name (substring,
	// case-insensitive).
	Text string
	// Role keeps users whose highest global role is Role (zero: any).
	Role Role
	// Enabled keeps enabled (true) or disabled (false) users; nil: both.
	Enabled *bool
	// NeverLoggedIn keeps users who never signed in.
	NeverLoggedIn bool
	// Offset skips the first users, by username (the previous pages).
	Offset int
	Limit  int
}

// InvitationRepository stores invitations.
type InvitationRepository interface {
	Add(ctx context.Context, i *Invitation) error
	// Save stores the redemption or revocation of an invitation.
	Save(ctx context.Context, i *Invitation) error
	// ByID returns an invitation or ErrInvitationNotFound.
	ByID(ctx context.Context, id InvitationID) (*Invitation, error)
	// ByTokenHash returns an invitation or ErrInvitationInvalid.
	ByTokenHash(ctx context.Context, h TokenHash) (*Invitation, error)
	// List returns the invitations, newest first.
	List(ctx context.Context, limit int) ([]*Invitation, error)
	// ClearEmailOfUser removes the address of the invitations a user
	// redeemed (account deletion, SR-64).
	ClearEmailOfUser(ctx context.Context, id UserID) error
	// DeleteEndedBefore deletes up to limit invitations that expired, were
	// redeemed or revoked before cutoff.
	DeleteEndedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error)
}

// PasswordResetRepository stores password reset tokens.
type PasswordResetRepository interface {
	// Add stores a token and invalidates the earlier unused tokens of its
	// user.
	Add(ctx context.Context, t *PasswordResetToken) error
	// Save stores the use of a token.
	Save(ctx context.Context, t *PasswordResetToken) error
	// ByTokenHash returns a token or ErrInvalidToken.
	ByTokenHash(ctx context.Context, h TokenHash) (*PasswordResetToken, error)
	// InvalidateForUser marks every unused token of a user used.
	InvalidateForUser(ctx context.Context, id UserID, now time.Time) error
	// DeleteEndedBefore deletes up to limit tokens that expired or were
	// used before cutoff.
	DeleteEndedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error)
}

// EmailChangeRepository stores e-mail change tokens.
type EmailChangeRepository interface {
	// Add stores a token and invalidates the earlier unused tokens of its
	// user.
	Add(ctx context.Context, t *EmailChangeToken) error
	Save(ctx context.Context, t *EmailChangeToken) error
	// ByTokenHash returns a token or ErrInvalidToken.
	ByTokenHash(ctx context.Context, h TokenHash) (*EmailChangeToken, error)
	// InvalidateForUser marks every unused token of a user used.
	InvalidateForUser(ctx context.Context, id UserID, now time.Time) error
	DeleteEndedBefore(ctx context.Context, cutoff time.Time, limit int) (int, error)
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
	// ActiveForUser returns the unrevoked, unexpired sessions of a user at
	// now, most recently seen first.
	ActiveForUser(ctx context.Context, id UserID, now time.Time) ([]*Session, error)
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

// AuditPurge is the retention side of the audit log: only the retention
// job uses it (TECHNICAL_SPEC §7.3 audit.purge).
type AuditPurge interface {
	// DeleteBefore deletes up to limit entries recorded before cutoff,
	// oldest first, and returns how many were deleted.
	DeleteBefore(ctx context.Context, cutoff time.Time, limit int) (int, error)
}

// AuditReader searches the audit log (ACC-010).
type AuditReader interface {
	// Search returns entries matching q, newest first.
	Search(ctx context.Context, q AuditQuery) ([]AuditRecord, error)
}

// AuditQuery filters the audit log.
type AuditQuery struct {
	ActorUserID  UserID
	ActorKind    ActorKind
	ActionPrefix string
	TargetType   string
	TargetID     string
	From, To     time.Time
	// BeforeID is the id of the last row of the previous page (0: first).
	BeforeID int64
	// Offset skips the newest matching entries (the previous pages).
	Offset int
	Limit  int
}

// AuditRecord is a stored audit entry with its id.
type AuditRecord struct {
	ID    int64
	Entry AuditEntry
}
