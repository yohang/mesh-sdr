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
	// a malformed hash.
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
	BlockedUntil(key string, now time.Time) time.Time
	RecordFailure(key string, now time.Time, p domain.ThrottlePolicy) bool
	Reset(key string)
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
