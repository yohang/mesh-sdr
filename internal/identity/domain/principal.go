package domain

import (
	"slices"
)

// Principal is who makes a request: anonymous, or a user through a session.
type Principal struct {
	userID    UserID
	sessionID SessionID
	username  Username
	display   DisplayName
	grants    []RoleGrant
	mustChPw  bool
}

// Anonymous returns the principal of a request without a valid session.
func Anonymous() Principal { return Principal{} }

// UserPrincipal returns the principal of a user's session.
func UserPrincipal(u *User, s *Session) Principal {
	return Principal{
		userID:    u.ID(),
		sessionID: s.ID(),
		username:  u.Username(),
		display:   u.DisplayName(),
		grants:    u.Grants(),
		mustChPw:  u.MustChangePassword(),
	}
}

// IsAnonymous reports whether there is no user.
func (p Principal) IsAnonymous() bool { return p.userID.IsZero() }

// UserID returns the user id (zero when anonymous).
func (p Principal) UserID() UserID { return p.userID }

// SessionID returns the session id (zero when anonymous).
func (p Principal) SessionID() SessionID { return p.sessionID }

// Username returns the username (zero when anonymous).
func (p Principal) Username() Username { return p.username }

// DisplayName returns the display name (zero when none).
func (p Principal) DisplayName() DisplayName { return p.display }

// MustChangePassword reports whether the user must set a new password.
func (p Principal) MustChangePassword() bool { return p.mustChPw }

// Grants returns the role grants, including device-scoped ones.
func (p Principal) Grants() []RoleGrant { return slices.Clone(p.grants) }

// Role returns the highest global role: anonymous, listener (every user),
// operator or admin.
func (p Principal) Role() Role {
	if p.IsAnonymous() {
		return RoleAnonymous
	}

	r := RoleListener
	for _, g := range p.grants {
		if g.Global() && g.role > r {
			r = g.role
		}
	}

	return r
}

// Roles returns the names of the roles held globally or on some device,
// lowest first (for the session API and access tokens).
func (p Principal) Roles() []Role {
	if p.IsAnonymous() {
		return nil
	}

	out := []Role{RoleListener}
	for _, g := range p.grants {
		if !slices.Contains(out, g.role) {
			out = append(out, g.role)
		}
	}

	slices.Sort(out)

	return out
}

// Has reports whether the principal holds role globally (rank order).
func (p Principal) Has(role Role) bool { return p.Role().Includes(role) }

// HasOnDevice reports whether the principal holds role on device, globally
// or through a grant scoped to it.
func (p Principal) HasOnDevice(role Role, device DeviceID) bool {
	if p.Has(role) {
		return true
	}

	for _, g := range p.grants {
		if g.device == device && g.role.Includes(role) {
			return true
		}
	}

	return false
}

// ListenPolicy is the effective listen policy of a device (TECHNICAL_SPEC
// §5.9): `devices.<id>.listen_policy` if set, otherwise the global one.
type ListenPolicy string

// Listen policies.
const (
	ListenAnonymous  ListenPolicy = "anonymous"
	ListenRegistered ListenPolicy = "registered"
)

// ParseListenPolicy validates a listen policy.
func ParseListenPolicy(s string) (ListenPolicy, error) {
	switch l := ListenPolicy(s); l {
	case ListenAnonymous, ListenRegistered:
		return l, nil
	}

	return "", ErrInvalidUser.WithDetail("invalid listen policy")
}

// CanListen reports whether the principal may listen to a device whose
// effective policy is lp. Resolving lp is the device registry's job.
func (p Principal) CanListen(lp ListenPolicy) bool {
	switch lp {
	case ListenAnonymous:
		return true
	case ListenRegistered:
		return !p.IsAnonymous()
	default:
		return false
	}
}
