// Package app holds the identity use cases: password login through an
// auth-provider interface, session resolution and logout, CLI user
// administration and the session reaper. Ports are declared here, on the
// consumer side; adapters live in internal/identity/infra.
package app

import (
	"context"
	"net/netip"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// PasswordHasher hashes and verifies local passwords (Argon2id).
type PasswordHasher interface {
	Hash(ctx context.Context, password string) (domain.PasswordHash, error)
	// Verify reports whether password matches hash; it returns an error for
	// a malformed hash, and an error matching domain.ErrRateLimited when
	// too many hashes are queued.
	Verify(ctx context.Context, password string, hash domain.PasswordHash) (bool, error)
	// NeedsRehash reports whether hash uses other parameters than the
	// configured ones.
	NeedsRehash(hash domain.PasswordHash) bool
}

// Transactor runs a unit of work in one write transaction.
type Transactor interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// IDGenerator returns new UUIDv7 surrogate keys.
type IDGenerator interface {
	New(now time.Time) (shared.UUID, error)
}

// Clock returns the current time.
type Clock func() time.Time

// IPLimiter rate-limits login attempts per client address.
type IPLimiter interface {
	Allow(ip netip.Addr, now time.Time) (bool, time.Duration)
}

// LoginThrottle throttles login identifiers that match no account.
type LoginThrottle interface {
	// Reserve atomically refuses an attempt for key while it is delayed or
	// locked (blocked, until when), or counts it as a failure (locked: this
	// failure locks the key).
	Reserve(key string, now time.Time, p domain.ThrottlePolicy) (blocked bool, until time.Time, locked bool)
	Reset(key string)
}

// RefusalGate bounds the audit of refused logins: only the first refusal
// of a key (client address, login identifier) within its blocking window
// is audited, so that a flood cannot turn into a stream of audit writes.
type RefusalGate interface {
	// First reports whether this refusal of key, blocked until until, is
	// the first one of its window.
	First(key string, until, now time.Time) bool
}

// Credentials are what a user submits to a provider. The local provider
// reads Login and Password.
type Credentials struct {
	Login    domain.Login
	Password string
}

// Provider turns credentials into a verified login identity (TECHNICAL_SPEC
// §5.2). The local form login is the only implementation in v1; OIDC
// providers will implement the same interface. Sessions never depend on the
// provider.
type Provider interface {
	ID() domain.ProviderID
	// Authenticate returns the verified identity, or ErrInvalidCredentials.
	Authenticate(ctx context.Context, c Credentials) (domain.Identity, error)
}

// RequestMeta describes the HTTP request or command behind a use case.
type RequestMeta struct {
	IP        netip.Addr
	UserAgent string
	RequestID string
}

// Settings are the identity settings that admins edit in the DB settings
// store (FEATURE_SPEC §9). Until the store is wired, an adapter returns the
// defaults.
type Settings interface {
	// PasswordMinLength is auth.password_min_length.
	PasswordMinLength(ctx context.Context) int
	// InvitationTTL is invitations.ttl_hours: the default validity of a new
	// invitation.
	InvitationTTL(ctx context.Context) time.Duration
	// PasswordResetTTL is password_reset.ttl_minutes.
	PasswordResetTTL(ctx context.Context) time.Duration
}

// Policies builds the password policy in force: the minimum length from the
// settings, the bundled common-password list.
type Policies struct {
	settings Settings
	common   domain.CommonPasswords
}

// NewPolicies returns the policy source. common may be nil (no list).
func NewPolicies(settings Settings, common domain.CommonPasswords) Policies {
	return Policies{settings: settings, common: common}
}

// Password returns the password policy in force.
func (p Policies) Password(ctx context.Context) domain.PasswordPolicy {
	minLength := domain.DefaultPasswordMinLength
	if p.settings != nil {
		minLength = p.settings.PasswordMinLength(ctx)
	}

	return domain.NewPasswordPolicy(minLength).WithCommonPasswords(p.common)
}

// SessionPolicies gives the current session lifetimes and per-account
// login throttling. The hub reads them from the settings store on every use
// (session.*, auth.lockout.*; ADR 0010), so a saved change applies to the
// next login or request.
type SessionPolicies interface {
	SessionPolicy() domain.SessionPolicy
	ThrottlePolicy() domain.ThrottlePolicy
}

// FixedSessionPolicies are constant policies (tests, defaults).
type FixedSessionPolicies struct {
	Session  domain.SessionPolicy
	Throttle domain.ThrottlePolicy
}

// DefaultSessionPolicies returns the built-in policies.
func DefaultSessionPolicies() FixedSessionPolicies {
	return FixedSessionPolicies{Session: domain.DefaultSessionPolicy(), Throttle: domain.DefaultThrottlePolicy()}
}

// SessionPolicy implements SessionPolicies.
func (p FixedSessionPolicies) SessionPolicy() domain.SessionPolicy { return p.Session }

// ThrottlePolicy implements SessionPolicies.
func (p FixedSessionPolicies) ThrottlePolicy() domain.ThrottlePolicy { return p.Throttle }
