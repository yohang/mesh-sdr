package domain

import (
	"regexp"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Role is one of the P3 roles (TECHNICAL_SPEC §5.1, `roles` seed). Its value
// is the `roles.id`; a higher rank implies every right of a lower rank.
type Role int16

// Roles. RoleAnonymous is the implicit role of a request without a session;
// it is never granted.
const (
	RoleAnonymous Role = 0
	RoleListener  Role = 10
	RoleOperator  Role = 20
	RoleAdmin     Role = 30
)

var roleNames = map[Role]string{
	RoleAnonymous: "anonymous",
	RoleListener:  "listener",
	RoleOperator:  "operator",
	RoleAdmin:     "admin",
}

// ParseRole returns the role named s.
func ParseRole(s string) (Role, error) {
	for r, n := range roleNames {
		if n == s {
			return r, nil
		}
	}

	return 0, ErrInvalidRole
}

// RoleFromID returns the role with the given `roles.id`.
func RoleFromID(i int64) (Role, error) {
	r := Role(i)
	if _, ok := roleNames[r]; !ok || int64(r) != i {
		return 0, ErrInvalidRole
	}

	return r, nil
}

// String returns the role name.
func (r Role) String() string {
	if n, ok := roleNames[r]; ok {
		return n
	}

	return "unknown"
}

// ID returns the `roles.id` of the role.
func (r Role) ID() int16 { return int16(r) }

// Includes reports whether r implies every right of o.
func (r Role) Includes(o Role) bool { return r >= o }

// RoleGrant is a granted role, global or limited to one device. Every
// registered user implicitly holds listener globally, so a grant is always
// operator (global or device-scoped) or admin (always global).
type RoleGrant struct {
	role   Role
	device shared.DeviceID
}

// NewRoleGrant validates a grant. A zero device means "all devices".
func NewRoleGrant(r Role, device shared.DeviceID) (RoleGrant, error) {
	switch {
	case r != RoleOperator && r != RoleAdmin:
		return RoleGrant{}, ErrInvalidRole.WithDetail("only operator and admin are granted (listener is implicit)")
	case r == RoleAdmin && !device.IsZero():
		return RoleGrant{}, ErrInvalidRole.WithDetail("admin is always global")
	}

	return RoleGrant{role: r, device: device}, nil
}

// Role returns the granted role.
func (g RoleGrant) Role() Role { return g.role }

// Device returns the device scope; zero means global.
func (g RoleGrant) Device() shared.DeviceID { return g.device }

// Global reports whether the grant applies to every device.
func (g RoleGrant) Global() bool { return g.device.IsZero() }

// ProviderID identifies an auth provider (TECHNICAL_SPEC §5.2): `local`
// today, later for example `oidc:<name>`.
type ProviderID struct{ v string }

var providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:_-]{0,31}$`)

// ProviderLocal is the form-login provider (username or e-mail + password).
var ProviderLocal = ProviderID{v: "local"}

// NewProviderID validates a provider id.
func NewProviderID(s string) (ProviderID, error) {
	if !providerPattern.MatchString(s) {
		return ProviderID{}, ErrInvalidIdentity.WithDetail("invalid provider id")
	}

	return ProviderID{v: s}, nil
}

// String returns the provider id.
func (p ProviderID) String() string { return p.v }

// Identity is one way a user authenticates: (provider, subject), unique.
type Identity struct {
	provider ProviderID
	subject  string
}

// NewIdentity validates an identity. The subject is the stable id at the
// provider (1 to 255 bytes).
func NewIdentity(p ProviderID, subject string) (Identity, error) {
	if p.v == "" || subject == "" || len(subject) > 255 {
		return Identity{}, ErrInvalidIdentity
	}

	return Identity{provider: p, subject: subject}, nil
}

// LocalIdentity returns the local identity of a user: its subject is the
// user id.
func LocalIdentity(u UserID) Identity {
	return Identity{provider: ProviderLocal, subject: u.String()}
}

// Provider returns the provider.
func (i Identity) Provider() ProviderID { return i.provider }

// Subject returns the subject at the provider.
func (i Identity) Subject() string { return i.subject }
