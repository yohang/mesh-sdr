package domain

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"
)

// tokenBytes is the entropy of session tokens and CSRF secrets (256 bits).
const tokenBytes = 32

// SessionToken is the secret session cookie value. Only its hash is stored.
type SessionToken struct{ v [tokenBytes]byte }

// NewSessionToken draws a token from the CSPRNG.
func NewSessionToken() SessionToken {
	var t SessionToken

	_, _ = rand.Read(t.v[:]) // never fails (crypto/rand, Go >= 1.24)

	return t
}

// ParseSessionToken parses the cookie form (base64url, no padding).
func ParseSessionToken(s string) (SessionToken, error) {
	var t SessionToken

	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != tokenBytes {
		return SessionToken{}, ErrInvalidToken
	}

	copy(t.v[:], b)

	return t, nil
}

// Cookie returns the cookie form of the token.
func (t SessionToken) Cookie() string { return base64.RawURLEncoding.EncodeToString(t.v[:]) }

// String redacts the token.
func (t SessionToken) String() string { return "<redacted>" }

// Hash returns the stored form: SHA-256 of the token bytes.
func (t SessionToken) Hash() TokenHash { return TokenHash{v: sha256.Sum256(t.v[:])} }

// TokenHash is the SHA-256 of a session token (`sessions.token_hash`).
type TokenHash struct{ v [sha256.Size]byte }

// NewTokenHash wraps a stored hash.
func NewTokenHash(b []byte) (TokenHash, error) {
	var h TokenHash
	if len(b) != sha256.Size {
		return TokenHash{}, ErrInvalidToken
	}

	copy(h.v[:], b)

	return h, nil
}

// Bytes returns the hash bytes.
func (h TokenHash) Bytes() []byte { return append([]byte(nil), h.v[:]...) }

// CSRFSecret is the per-session CSRF secret (`sessions.csrf_secret`). The
// CSRF token is HMAC-SHA256(secret, raw session token), so the stored secret
// alone cannot forge a token: the raw token is never stored.
type CSRFSecret struct{ v [tokenBytes]byte }

// NewCSRFSecret draws a secret from the CSPRNG.
func NewCSRFSecret() CSRFSecret {
	var s CSRFSecret

	_, _ = rand.Read(s.v[:])

	return s
}

// RehydrateCSRFSecret wraps a stored secret.
func RehydrateCSRFSecret(b []byte) (CSRFSecret, error) {
	var s CSRFSecret
	if len(b) != tokenBytes {
		return CSRFSecret{}, ErrInvalidSession.WithDetail("invalid CSRF secret")
	}

	copy(s.v[:], b)

	return s, nil
}

// Bytes returns the secret bytes.
func (s CSRFSecret) Bytes() []byte { return append([]byte(nil), s.v[:]...) }

// Token returns the CSRF token of the session that token opens.
func (s CSRFSecret) Token(token SessionToken) string {
	mac := hmac.New(sha256.New, s.v[:])
	mac.Write(token.v[:])

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify checks a CSRF token in constant time.
func (s CSRFSecret) Verify(token SessionToken, csrf string) bool {
	return csrf != "" && hmac.Equal([]byte(csrf), []byte(s.Token(token)))
}

// RevokeReason is why a session ended (`sessions.revoke_reason`).
type RevokeReason string

// Revoke reasons. RevokeRotated is not in the spec list: the session a
// browser presented when it logged in again (session rotation).
const (
	RevokeLogout         RevokeReason = "logout"
	RevokeAdmin          RevokeReason = "admin"
	RevokePasswordChange RevokeReason = "password_change"
	RevokePasswordReset  RevokeReason = "password_reset"
	RevokeUserDisabled   RevokeReason = "user_disabled"
	RevokeExpired        RevokeReason = "expired"
	RevokeRotated        RevokeReason = "rotated"
)

// ParseRevokeReason validates a reason.
func ParseRevokeReason(s string) (RevokeReason, error) {
	switch r := RevokeReason(s); r {
	case RevokeLogout, RevokeAdmin, RevokePasswordChange, RevokePasswordReset, RevokeUserDisabled, RevokeExpired, RevokeRotated:
		return r, nil
	}

	return "", ErrInvalidSession.WithDetail("invalid revoke reason")
}

// SessionPolicy holds the session lifetimes (AUTH-003).
type SessionPolicy struct {
	idle       time.Duration
	absolute   time.Duration
	remember   time.Duration
	touchEvery time.Duration
}

// DefaultSessionPolicy: idle 24 h, absolute 24 h, or 30 days with "remember
// me"; last_seen_at is written at most once per minute.
func DefaultSessionPolicy() SessionPolicy {
	return SessionPolicy{idle: 24 * time.Hour, absolute: 24 * time.Hour, remember: 30 * 24 * time.Hour, touchEvery: time.Minute}
}

// NewSessionPolicy returns a policy; zero values keep the defaults.
func NewSessionPolicy(idle, absolute, remember time.Duration) SessionPolicy {
	p := DefaultSessionPolicy()
	if idle > 0 {
		p.idle = idle
	}

	if absolute > 0 {
		p.absolute = absolute
	}

	if remember > 0 {
		p.remember = remember
	}

	return p
}

// Lifetime returns the absolute lifetime of a new session.
func (p SessionPolicy) Lifetime(remember bool) time.Duration {
	if remember {
		return p.remember
	}

	return p.absolute
}

// Session is a browser session (TECHNICAL_SPEC §5.5, §7.1 `sessions`).
type Session struct {
	id                SessionID
	tokenHash         TokenHash
	csrfSecret        CSRFSecret
	userID            UserID
	provider          ProviderID
	createdAt         time.Time
	lastSeenAt        time.Time
	idleExpiresAt     time.Time
	absoluteExpiresAt time.Time
	ip                netip.Addr
	userAgent         string
	revokedAt         time.Time
	revokeReason      RevokeReason
}

// StartSessionParams is the input of StartSession.
type StartSessionParams struct {
	ID        SessionID
	UserID    UserID
	Provider  ProviderID
	Remember  bool
	IP        netip.Addr // optional
	UserAgent string     // optional, truncated to 512 bytes
	Policy    SessionPolicy
	Now       time.Time
	// NotAfter, when set, caps the absolute expiry: a session that replaces
	// another (rotation) keeps the replaced session's absolute lifetime.
	NotAfter time.Time
}

// StartSession opens a session and returns it with its secret token, to be
// sent once in the cookie.
func StartSession(p StartSessionParams) (*Session, SessionToken, error) {
	if p.ID.IsZero() || p.UserID.IsZero() || p.Provider.v == "" || p.Now.IsZero() {
		return nil, SessionToken{}, ErrInvalidSession
	}

	pol := p.Policy
	if pol.idle == 0 {
		pol = DefaultSessionPolicy()
	}

	now := p.Now.UTC().Truncate(time.Millisecond)
	token := NewSessionToken()
	abs := now.Add(pol.Lifetime(p.Remember))
	if !p.NotAfter.IsZero() && p.NotAfter.Before(abs) {
		abs = p.NotAfter.UTC().Truncate(time.Millisecond)
	}

	if !abs.After(now) {
		return nil, SessionToken{}, ErrInvalidSession.WithDetail("the session would already be expired")
	}

	return &Session{
		id:                p.ID,
		tokenHash:         token.Hash(),
		csrfSecret:        NewCSRFSecret(),
		userID:            p.UserID,
		provider:          p.Provider,
		createdAt:         now,
		lastSeenAt:        now,
		idleExpiresAt:     earliest(now.Add(pol.idle), abs),
		absoluteExpiresAt: abs,
		ip:                canonicalIP(p.IP),
		userAgent:         truncateUTF8(p.UserAgent, 512),
	}, token, nil
}

func canonicalIP(a netip.Addr) netip.Addr {
	if !a.IsValid() {
		return a
	}

	return a.Unmap().WithZone("")
}

func truncateUTF8(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}

// SessionState is the persisted state of a session, for rehydration.
type SessionState struct {
	ID                SessionID
	TokenHash         TokenHash
	CSRFSecret        CSRFSecret
	UserID            UserID
	Provider          ProviderID
	CreatedAt         time.Time
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
	IP                netip.Addr
	UserAgent         string
	RevokedAt         time.Time
	RevokeReason      RevokeReason
}

// RehydrateSession rebuilds a stored session.
func RehydrateSession(s SessionState) (*Session, error) {
	if s.ID.IsZero() || s.UserID.IsZero() || s.Provider.v == "" {
		return nil, ErrInvalidSession
	}

	return &Session{
		id: s.ID, tokenHash: s.TokenHash, csrfSecret: s.CSRFSecret, userID: s.UserID, provider: s.Provider,
		createdAt: s.CreatedAt, lastSeenAt: s.LastSeenAt, idleExpiresAt: s.IdleExpiresAt,
		absoluteExpiresAt: s.AbsoluteExpiresAt, ip: canonicalIP(s.IP), userAgent: s.UserAgent,
		revokedAt: s.RevokedAt, revokeReason: s.RevokeReason,
	}, nil
}

// ID returns the session id.
func (s *Session) ID() SessionID { return s.id }

// TokenHash returns the hash of the cookie value.
func (s *Session) TokenHash() TokenHash { return s.tokenHash }

// CSRFSecret returns the CSRF secret.
func (s *Session) CSRFSecret() CSRFSecret { return s.csrfSecret }

// UserID returns the owner.
func (s *Session) UserID() UserID { return s.userID }

// Provider returns the provider used to log in (audit only).
func (s *Session) Provider() ProviderID { return s.provider }

// CreatedAt returns the login time.
func (s *Session) CreatedAt() time.Time { return s.createdAt }

// LastSeenAt returns the last recorded activity.
func (s *Session) LastSeenAt() time.Time { return s.lastSeenAt }

// IdleExpiresAt returns the idle expiry.
func (s *Session) IdleExpiresAt() time.Time { return s.idleExpiresAt }

// AbsoluteExpiresAt returns the absolute expiry.
func (s *Session) AbsoluteExpiresAt() time.Time { return s.absoluteExpiresAt }

// IP returns the client address at login (invalid when unknown).
func (s *Session) IP() netip.Addr { return s.ip }

// UserAgent returns the user agent at login.
func (s *Session) UserAgent() string { return s.userAgent }

// RevokedAt returns the revocation time (zero when not revoked).
func (s *Session) RevokedAt() time.Time { return s.revokedAt }

// RevokeReason returns why the session was revoked.
func (s *Session) RevokeReason() RevokeReason { return s.revokeReason }

// Ref returns the public handle of the session: what users and admins see
// to revoke it, and the `sid` claim of access tokens. The session id itself
// is never sent to clients (§7.1).
func (s *Session) Ref() string {
	h := sha256.Sum256(append([]byte("rx-sid:"), s.id.Bytes()...))

	return base64.RawURLEncoding.EncodeToString(h[:16])
}

// Persistent reports whether the session was opened with "remember me": its
// absolute lifetime exceeds the policy's plain lifetime.
func (s *Session) Persistent(p SessionPolicy) bool {
	return s.absoluteExpiresAt.Sub(s.createdAt) > p.Lifetime(false)
}

// ActiveAt reports whether the session is valid at now: not revoked and
// neither idle nor absolute expiry reached.
func (s *Session) ActiveAt(now time.Time) bool {
	return s.revokedAt.IsZero() && now.Before(s.idleExpiresAt) && now.Before(s.absoluteExpiresAt)
}

// Touch records activity. It returns true when the change must be stored:
// writes are coalesced to at most one per policy interval.
func (s *Session) Touch(now time.Time, p SessionPolicy) bool {
	if p.idle == 0 {
		p = DefaultSessionPolicy()
	}

	if !s.ActiveAt(now) || now.Sub(s.lastSeenAt) < p.touchEvery {
		return false
	}

	now = now.UTC().Truncate(time.Millisecond)
	s.lastSeenAt = now
	s.idleExpiresAt = earliest(now.Add(p.idle), s.absoluteExpiresAt)

	return true
}

// Revoke ends the session. It returns false when it was already revoked.
func (s *Session) Revoke(reason RevokeReason, now time.Time) bool {
	if !s.revokedAt.IsZero() {
		return false
	}

	s.revokedAt = now.UTC().Truncate(time.Millisecond)
	s.revokeReason = reason

	return true
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}

	return a
}
