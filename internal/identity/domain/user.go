package domain

import (
	"slices"
	"time"
)

// Origin tells where a user comes from (`users.origin`).
type Origin string

// Origins.
const (
	OriginConfig Origin = "config"
	OriginDB     Origin = "db"
	OriginImport Origin = "import"
)

// ParseOrigin validates an origin.
func ParseOrigin(s string) (Origin, error) {
	switch o := Origin(s); o {
	case OriginConfig, OriginDB, OriginImport:
		return o, nil
	}

	return "", ErrInvalidUser.WithDetail("invalid origin")
}

// User is the account aggregate: the person, its login identities and its
// role grants (TECHNICAL_SPEC §5.1, §7.1 `users`, `user_identities`,
// `user_roles`).
type User struct {
	id                 UserID
	username           Username
	email              Email
	emailVerifiedAt    time.Time
	displayName        DisplayName
	passwordHash       PasswordHash
	mustChangePassword bool
	enabled            bool
	failedLogins       int
	lockedUntil        time.Time
	lastLoginAt        time.Time
	origin             Origin
	createdAt          time.Time
	updatedAt          time.Time
	version            int
	identities         []Identity
	grants             []RoleGrant
}

// NewLocalUser is the input of NewLocalUser.
type NewLocalUserParams struct {
	ID                 UserID
	Username           Username
	Email              Email       // optional
	DisplayName        DisplayName // optional
	PasswordHash       PasswordHash
	MustChangePassword bool
	Grants             []RoleGrant
	Origin             Origin
	Now                time.Time
}

// NewLocalUser creates an enabled account with a local identity (subject =
// the user id) and a password.
func NewLocalUser(p NewLocalUserParams) (*User, error) {
	switch {
	case p.ID.IsZero(), p.Username.IsZero(), p.Now.IsZero():
		return nil, ErrInvalidUser
	case p.PasswordHash.IsZero():
		return nil, ErrInvalidUser.WithDetail("a local user needs a password")
	}

	origin := p.Origin
	if origin == "" {
		origin = OriginDB
	}

	if _, err := ParseOrigin(string(origin)); err != nil {
		return nil, err
	}

	now := p.Now.UTC().Truncate(time.Millisecond)

	return &User{
		id:                 p.ID,
		username:           p.Username,
		email:              p.Email,
		displayName:        p.DisplayName,
		passwordHash:       p.PasswordHash,
		mustChangePassword: p.MustChangePassword,
		enabled:            true,
		origin:             origin,
		createdAt:          now,
		updatedAt:          now,
		version:            1,
		identities:         []Identity{LocalIdentity(p.ID)},
		grants:             dedupGrants(p.Grants),
	}, nil
}

func dedupGrants(in []RoleGrant) []RoleGrant {
	out := make([]RoleGrant, 0, len(in))
	for _, g := range in {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}

	return out
}

// UserState is the persisted state of a user, for rehydration by
// repositories. Every field is already a valid value object.
type UserState struct {
	ID                 UserID
	Username           Username
	Email              Email
	EmailVerifiedAt    time.Time
	DisplayName        DisplayName
	PasswordHash       PasswordHash
	MustChangePassword bool
	Enabled            bool
	FailedLogins       int
	LockedUntil        time.Time
	LastLoginAt        time.Time
	Origin             Origin
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Version            int
	Identities         []Identity
	Grants             []RoleGrant
}

// RehydrateUser rebuilds a stored user.
func RehydrateUser(s UserState) (*User, error) {
	if s.ID.IsZero() || s.Username.IsZero() || s.Version < 1 || s.FailedLogins < 0 {
		return nil, ErrInvalidUser
	}

	if _, err := ParseOrigin(string(s.Origin)); err != nil {
		return nil, err
	}

	return &User{
		id: s.ID, username: s.Username, email: s.Email, emailVerifiedAt: s.EmailVerifiedAt,
		displayName: s.DisplayName, passwordHash: s.PasswordHash, mustChangePassword: s.MustChangePassword,
		enabled: s.Enabled, failedLogins: s.FailedLogins, lockedUntil: s.LockedUntil, lastLoginAt: s.LastLoginAt,
		origin: s.Origin, createdAt: s.CreatedAt, updatedAt: s.UpdatedAt, version: s.Version,
		identities: slices.Clone(s.Identities), grants: dedupGrants(s.Grants),
	}, nil
}

// ID returns the user id.
func (u *User) ID() UserID { return u.id }

// Username returns the username.
func (u *User) Username() Username { return u.username }

// Email returns the e-mail address (zero when none).
func (u *User) Email() Email { return u.email }

// EmailVerifiedAt returns when the e-mail was verified (zero when not).
func (u *User) EmailVerifiedAt() time.Time { return u.emailVerifiedAt }

// DisplayName returns the display name (zero when none).
func (u *User) DisplayName() DisplayName { return u.displayName }

// PasswordHash returns the local password hash (zero when none).
func (u *User) PasswordHash() PasswordHash { return u.passwordHash }

// MustChangePassword reports whether the user must set a new password.
func (u *User) MustChangePassword() bool { return u.mustChangePassword }

// Enabled reports whether the account may sign in.
func (u *User) Enabled() bool { return u.enabled }

// FailedLogins returns the number of consecutive failed logins.
func (u *User) FailedLogins() int { return u.failedLogins }

// LockedUntil returns when the next login attempt is allowed (zero: now).
func (u *User) LockedUntil() time.Time { return u.lockedUntil }

// LastLoginAt returns the last successful login (zero: never).
func (u *User) LastLoginAt() time.Time { return u.lastLoginAt }

// Origin returns where the user comes from.
func (u *User) Origin() Origin { return u.origin }

// CreatedAt returns the creation time.
func (u *User) CreatedAt() time.Time { return u.createdAt }

// UpdatedAt returns the last change time.
func (u *User) UpdatedAt() time.Time { return u.updatedAt }

// Version returns the optimistic-concurrency version.
func (u *User) Version() int { return u.version }

// Identities returns the login identities.
func (u *User) Identities() []Identity { return slices.Clone(u.identities) }

// Grants returns the role grants (listener is implicit).
func (u *User) Grants() []RoleGrant { return slices.Clone(u.grants) }

// HasLocalIdentity reports whether the user has a `local` identity.
func (u *User) HasLocalIdentity() bool {
	return slices.ContainsFunc(u.identities, func(i Identity) bool { return i.provider == ProviderLocal })
}

// CanPasswordLogin reports whether password login is possible for this
// account: a local identity and a password hash (SR-10). It does not check
// that the account is enabled.
func (u *User) CanPasswordLogin() bool {
	return u.HasLocalIdentity() && !u.passwordHash.IsZero()
}

// Role returns the highest global role of the user (at least listener).
func (u *User) Role() Role {
	r := RoleListener
	for _, g := range u.grants {
		if g.Global() && g.role > r {
			r = g.role
		}
	}

	return r
}

// BlockedAt reports whether login attempts are refused at now (progressive
// delay or lock-out), and until when.
func (u *User) BlockedAt(now time.Time) (bool, time.Time) {
	return u.lockedUntil.After(now), u.lockedUntil
}

// RecordLoginFailure counts a failed password and applies the throttle
// policy. It returns whether the account is now locked out.
func (u *User) RecordLoginFailure(now time.Time, p ThrottlePolicy) bool {
	if u.failedLogins < 32767 {
		u.failedLogins++
	}

	u.lockedUntil = p.BlockedUntil(u.failedLogins, now).UTC().Truncate(time.Millisecond)
	u.touch(now)

	return p.Locks(u.failedLogins)
}

// RecordLoginSuccess resets the throttling and records the login time.
func (u *User) RecordLoginSuccess(now time.Time) {
	u.failedLogins = 0
	u.lockedUntil = time.Time{}
	u.lastLoginAt = now.UTC().Truncate(time.Millisecond)
	u.touch(now)
}

// ReplacePasswordHash stores a new hash of the same password (re-hash with
// the current parameters). It does not count as a password change.
func (u *User) ReplacePasswordHash(h PasswordHash, now time.Time) error {
	if h.IsZero() {
		return ErrInvalidHash
	}

	u.passwordHash = h
	u.touch(now)

	return nil
}

// ChangePassword sets a new password chosen by the user: it clears
// must_change_password and the login throttling.
func (u *User) ChangePassword(h PasswordHash, now time.Time) error {
	if h.IsZero() {
		return ErrInvalidHash
	}

	if !u.HasLocalIdentity() {
		return ErrInvalidUser.WithDetail("the user has no local identity")
	}

	u.passwordHash = h
	u.mustChangePassword = false
	u.ClearLoginFailures(now)

	return nil
}

// ResetPassword sets a password chosen for the user (CLI or admin reset):
// mustChange flags a generated password that the user must replace at the
// next sign-in. It clears the login throttling, so that a locked-out user
// can sign in with the new password.
func (u *User) ResetPassword(h PasswordHash, mustChange bool, now time.Time) error {
	if h.IsZero() {
		return ErrInvalidHash
	}

	if !u.HasLocalIdentity() {
		return ErrInvalidUser.WithDetail("the user has no local identity")
	}

	u.passwordHash = h
	u.mustChangePassword = mustChange
	u.ClearLoginFailures(now)

	return nil
}

// ClearLoginFailures resets the login throttling (after a password check
// other than a login succeeded, or a password reset).
func (u *User) ClearLoginFailures(now time.Time) {
	u.failedLogins = 0
	u.lockedUntil = time.Time{}
	u.touch(now)
}

// Disable disables the account. The caller revokes its sessions. It
// returns false when the account was already disabled.
func (u *User) Disable(now time.Time) bool {
	if !u.enabled {
		return false
	}

	u.enabled = false
	u.touch(now)

	return true
}

// Enable enables the account. It returns false when it was already enabled.
func (u *User) Enable(now time.Time) bool {
	if u.enabled {
		return false
	}

	u.enabled = true
	u.failedLogins = 0
	u.lockedUntil = time.Time{}
	u.touch(now)

	return true
}

func (u *User) touch(now time.Time) {
	u.updatedAt = now.UTC().Truncate(time.Millisecond)
}

// Saved records that the repository stored the aggregate: its version is
// incremented. Repositories call it after a successful update.
func (u *User) Saved() { u.version++ }
