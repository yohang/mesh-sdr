// Package settingsrc reads the identity policies from the settings store
// (ADR 0010): session lifetimes, login throttling and retention. Values are
// read on every use, so a saved change applies at once. The store validates
// them (bounds and checks across keys); invalid combinations fall back to
// the domain defaults.
package settingsrc

import (
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Setting keys read by the identity module.
const (
	KeySessionIdle      = "session.idle_timeout"
	KeySessionAbsolute  = "session.absolute_timeout"
	KeySessionRemember  = "session.remember_me_timeout"
	KeyLoginRateLimit   = "auth.login_rate_limit"
	KeyLockoutDelay     = "auth.lockout.delay_after"
	KeyLockoutLockAfter = "auth.lockout.lock_after"
	KeyLockoutLockFor   = "auth.lockout.lock_for"
	KeyLockoutMaxLock   = "auth.lockout.max_lock"
	KeyRetentionSession = "retention.sessions"
	KeyRetentionAudit   = "retention.audit_log"
)

// Default login rate per client address (TECHNICAL_SPEC §5.12): 5 per
// minute, used when the setting is unavailable.
const (
	defaultLoginBurst = 5
	defaultLoginEvery = 12 * time.Second
)

// Minimum audit retention (TECHNICAL_SPEC §7.3 audit.purge).
const minAuditRetention = 30 * 24 * time.Hour

// Values reads the current effective settings.
type Values interface {
	Int(key string) int
	Duration(key string) time.Duration
	Rate(key string) (int, time.Duration)
}

// Policies adapts the settings store to the identity ports.
type Policies struct{ values Values }

var _ app.SessionPolicies = Policies{}

// New returns the adapter.
func New(values Values) Policies { return Policies{values: values} }

// SessionPolicy implements app.SessionPolicies.
func (p Policies) SessionPolicy() domain.SessionPolicy {
	return domain.NewSessionPolicy(p.values.Duration(KeySessionIdle), p.values.Duration(KeySessionAbsolute),
		p.values.Duration(KeySessionRemember))
}

// ThrottlePolicy implements app.SessionPolicies.
func (p Policies) ThrottlePolicy() domain.ThrottlePolicy {
	return domain.NewThrottlePolicy(p.values.Int(KeyLockoutDelay), p.values.Int(KeyLockoutLockAfter),
		p.values.Duration(KeyLockoutLockFor), p.values.Duration(KeyLockoutMaxLock))
}

// LoginRate returns the per-address login rate: burst attempts, then one
// per every (auth.login_rate_limit = "<burst>/<window>").
func (p Policies) LoginRate() (every time.Duration, burst int) {
	n, window := p.values.Rate(KeyLoginRateLimit)
	if n <= 0 || window <= 0 {
		return defaultLoginEvery, defaultLoginBurst
	}

	return window / time.Duration(n), n
}

// SessionRetention implements app.Retention: ended sessions are kept this
// long.
func (p Policies) SessionRetention() time.Duration {
	if d := p.values.Duration(KeyRetentionSession); d > 0 {
		return d
	}

	return app.DefaultSessionRetention
}

// AuditRetention implements app.Retention: audit entries are kept this long
// (never less than 30 days).
func (p Policies) AuditRetention() time.Duration {
	return max(p.values.Duration(KeyRetentionAudit), minAuditRetention)
}
