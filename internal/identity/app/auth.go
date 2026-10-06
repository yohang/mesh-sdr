package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Auth runs logins, resolves sessions and logs out.
type Auth struct {
	users      domain.UserRepository
	sessions   domain.SessionRepository
	audit      domain.AuditLog
	tx         Transactor
	ids        IDGenerator
	now        Clock
	provider   Provider
	ipLimit    IPLimiter
	unknown    LoginThrottle
	throttle   domain.ThrottlePolicy
	sessionPol domain.SessionPolicy
	logger     *slog.Logger
}

// AuthDeps are the dependencies of Auth.
type AuthDeps struct {
	Users         domain.UserRepository
	Sessions      domain.SessionRepository
	Audit         domain.AuditLog
	Tx            Transactor
	IDs           IDGenerator
	Now           Clock
	Provider      Provider
	IPLimiter     IPLimiter
	Unknown       LoginThrottle
	Throttle      domain.ThrottlePolicy
	SessionPolicy domain.SessionPolicy
	Logger        *slog.Logger
}

// NewAuth returns the service.
func NewAuth(d AuthDeps) *Auth {
	return &Auth{
		users: d.Users, sessions: d.Sessions, audit: d.Audit, tx: d.Tx, ids: d.IDs, now: d.Now,
		provider: d.Provider, ipLimit: d.IPLimiter, unknown: d.Unknown, throttle: d.Throttle,
		sessionPol: d.SessionPolicy, logger: d.Logger,
	}
}

// SessionPolicy returns the session lifetimes.
func (a *Auth) SessionPolicy() domain.SessionPolicy { return a.sessionPol }

// LoginInput is a password login attempt.
type LoginInput struct {
	Login    string
	Password string
	Remember bool
	// Previous is the session cookie the browser presented, if any: it is
	// revoked (session rotation).
	Previous string
	Meta     RequestMeta
}

// LoginResult is an opened session.
type LoginResult struct {
	Token     domain.SessionToken
	Session   *domain.Session
	Principal domain.Principal
	Remember  bool
}

// Login checks the rate limits, authenticates through the provider,
// resolves the identity to an enabled user and opens a new session. Every
// failure returns ErrInvalidCredentials or a *domain.RateLimitError, the
// same for existing and unknown accounts.
func (a *Auth) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	now := a.now()

	if ok, wait := a.ipLimit.Allow(in.Meta.IP, now); !ok {
		a.auditFailure(ctx, in.Meta, nil, "ip_rate_limited")

		return LoginResult{}, domain.NewRateLimitError(wait)
	}

	login, err := domain.NewLogin(in.Login)
	if err != nil {
		// Still pay for one verification: same timing as a wrong password.
		login, _ = domain.NewLogin("-")
	}

	known, err := a.users.ByLogin(ctx, login)
	if err != nil && !errors.Is(err, domain.ErrUserNotFound) {
		return LoginResult{}, fmt.Errorf("load user: %w", err)
	}

	// Reserve the attempt before verifying the password: the failure is
	// counted first and reset on success, so parallel guesses cannot exceed
	// the throttle thresholds.
	r, err := a.reserve(ctx, known, login, now)
	if err != nil {
		return LoginResult{}, err
	}

	if r.blocked {
		a.auditFailure(ctx, in.Meta, known, "throttled")

		return LoginResult{}, domain.NewRateLimitError(r.until.Sub(now))
	}

	ident, err := a.provider.Authenticate(ctx, Credentials{Login: login, Password: in.Password})
	if errors.Is(err, domain.ErrInvalidCredentials) {
		a.auditFailure(ctx, in.Meta, known, "invalid_credentials")

		if r.locked {
			a.record(ctx, in.Meta, known, domain.ActionLoginLockout, domain.ResultDenied, nil)
		}

		return LoginResult{}, domain.ErrInvalidCredentials
	}

	if errors.Is(err, domain.ErrRateLimited) {
		return LoginResult{}, err // password hashing saturated
	}

	if err != nil {
		return LoginResult{}, fmt.Errorf("authenticate: %w", err)
	}

	u, err := a.users.ByIdentity(ctx, ident)
	if errors.Is(err, domain.ErrUserNotFound) {
		// An unknown identity never creates an account (§5.2).
		a.auditFailure(ctx, in.Meta, nil, "unknown_identity")

		return LoginResult{}, domain.ErrInvalidCredentials
	}

	if err != nil {
		return LoginResult{}, fmt.Errorf("resolve identity: %w", err)
	}

	if !u.Enabled() {
		a.auditFailure(ctx, in.Meta, u, "disabled")

		return LoginResult{}, domain.ErrInvalidCredentials
	}

	return a.open(ctx, u.ID(), login, in, now)
}

// reservation is the outcome of reserving a login attempt.
type reservation struct {
	blocked bool      // refused: delayed or locked out
	until   time.Time // when the next attempt is allowed, if blocked
	locked  bool      // this attempt, if it fails, locks the account
}

// reserve atomically checks the throttle of the account (persisted) or of
// the unknown identifier (in memory) and counts the attempt as a failure;
// a successful login resets the count.
func (a *Auth) reserve(ctx context.Context, known *domain.User, login domain.Login, now time.Time) (reservation, error) {
	if known == nil {
		blocked, until, locked := a.unknown.Reserve(login.Key(), now, a.throttle)

		return reservation{blocked: blocked, until: until, locked: locked}, nil
	}

	var r reservation

	err := a.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := a.users.ByID(ctx, known.ID())
		if err != nil {
			return err
		}

		if blocked, until := u.BlockedAt(now); blocked {
			r = reservation{blocked: true, until: until}

			return nil
		}

		r.locked = u.RecordLoginFailure(now, a.throttle)

		return a.users.Save(ctx, u)
	})
	if err != nil {
		return reservation{}, fmt.Errorf("reserve login attempt: %w", err)
	}

	return r, nil
}

func (a *Auth) open(ctx context.Context, id domain.UserID, login domain.Login, in LoginInput, now time.Time) (LoginResult, error) {
	var res LoginResult

	err := a.tx.WithinTx(ctx, func(ctx context.Context) error {
		u, err := a.users.ByID(ctx, id)
		if err != nil {
			return err
		}

		u.RecordLoginSuccess(now)

		if err := a.users.Save(ctx, u); err != nil {
			return err
		}

		if err := a.revokePrevious(ctx, in.Previous, now); err != nil {
			return err
		}

		sid, err := a.ids.New(now)
		if err != nil {
			return fmt.Errorf("session id: %w", err)
		}

		sessionID, err := domain.NewSessionID(sid)
		if err != nil {
			return err
		}

		s, token, err := domain.StartSession(domain.StartSessionParams{
			ID: sessionID, UserID: u.ID(), Provider: a.provider.ID(), Remember: in.Remember,
			IP: in.Meta.IP, UserAgent: in.Meta.UserAgent, Policy: a.sessionPol, Now: now,
		})
		if err != nil {
			return err
		}

		if err := a.sessions.Add(ctx, s); err != nil {
			return err
		}

		e, err := domain.NewAuditEntry(now, domain.UserActor(u.ID(), in.Meta.IP), domain.ActionLoginSuccess, domain.ResultOK)
		if err != nil {
			return err
		}

		e = e.WithTarget("user", u.ID().String()).WithRequestID(in.Meta.RequestID).
			WithAfter(map[string]string{"provider": a.provider.ID().String(), "remember_me": strconv.FormatBool(in.Remember)})

		if err := a.audit.Append(ctx, e); err != nil {
			return err
		}

		res = LoginResult{Token: token, Session: s, Principal: domain.UserPrincipal(u, s), Remember: in.Remember}

		return nil
	})
	if err != nil {
		return LoginResult{}, fmt.Errorf("open session: %w", err)
	}

	a.unknown.Reset(login.Key())
	a.logger.InfoContext(ctx, "user logged in", slog.String("user_id", id.String()), slog.String("provider", a.provider.ID().String()))

	return res, nil
}

func (a *Auth) revokePrevious(ctx context.Context, cookie string, now time.Time) error {
	if cookie == "" {
		return nil
	}

	tok, err := domain.ParseSessionToken(cookie)
	if err != nil {
		return nil // a malformed cookie has no session to revoke
	}

	s, err := a.sessions.ByTokenHash(ctx, tok.Hash())
	if errors.Is(err, domain.ErrSessionNotFound) {
		return nil
	}

	if err != nil {
		return err
	}

	if s.Revoke(domain.RevokeRotated, now) {
		return a.sessions.Revoke(ctx, s)
	}

	return nil
}

// auditFailure records a failed login. Audit failures of failed logins are
// logged, not returned: the caller still gets the generic error.
func (a *Auth) auditFailure(ctx context.Context, meta RequestMeta, u *domain.User, reason string) {
	action := domain.ActionLoginFailure
	if reason == "locked" {
		action = domain.ActionLoginLockout
	}

	a.record(ctx, meta, u, action, domain.ResultDenied, map[string]string{"provider": a.provider.ID().String(), "reason": reason})
}

func (a *Auth) record(ctx context.Context, meta RequestMeta, u *domain.User, action string, result domain.AuditResult, after map[string]string) {
	e, err := domain.NewAuditEntry(a.now(), domain.AnonymousActor(meta.IP), action, result)
	if err == nil {
		e = e.WithRequestID(meta.RequestID).WithAfter(after)
		if u != nil {
			e = e.WithTarget("user", u.ID().String())
		}

		err = a.audit.Append(ctx, e)
	}

	if err != nil {
		a.logger.WarnContext(ctx, "audit entry not written", slog.String("action", action), slog.Any("error", err))
	}
}

// Resolution is the session a request presents.
type Resolution struct {
	Principal domain.Principal
	Session   *domain.Session
	Token     domain.SessionToken
}

// Resolve returns the session of a cookie value. It returns ErrUnauthenticated
// for a malformed, unknown, revoked or expired session, or a disabled user.
// Activity is recorded at most once per minute.
func (a *Auth) Resolve(ctx context.Context, cookie string) (Resolution, error) {
	tok, err := domain.ParseSessionToken(cookie)
	if err != nil {
		return Resolution{}, domain.ErrUnauthenticated
	}

	s, err := a.sessions.ByTokenHash(ctx, tok.Hash())
	if errors.Is(err, domain.ErrSessionNotFound) {
		return Resolution{}, domain.ErrUnauthenticated
	}

	if err != nil {
		return Resolution{}, fmt.Errorf("load session: %w", err)
	}

	now := a.now()
	if !s.ActiveAt(now) {
		return Resolution{}, domain.ErrUnauthenticated
	}

	u, err := a.users.ByID(ctx, s.UserID())
	if errors.Is(err, domain.ErrUserNotFound) {
		return Resolution{}, domain.ErrUnauthenticated
	}

	if err != nil {
		return Resolution{}, fmt.Errorf("load session user: %w", err)
	}

	if !u.Enabled() {
		return Resolution{}, domain.ErrUnauthenticated
	}

	if s.Touch(now, a.sessionPol) {
		if err := a.sessions.Touch(ctx, s); err != nil {
			// Activity tracking is best effort; the session stays valid.
			a.logger.WarnContext(ctx, "session activity not recorded", slog.String("session_id", s.ID().String()), slog.Any("error", err))
		}
	}

	return Resolution{Principal: domain.UserPrincipal(u, s), Session: s, Token: tok}, nil
}

// Logout revokes the session of a cookie value. It is idempotent.
func (a *Auth) Logout(ctx context.Context, cookie string, meta RequestMeta) error {
	tok, err := domain.ParseSessionToken(cookie)
	if err != nil {
		return nil // nothing to log out
	}

	return a.tx.WithinTx(ctx, func(ctx context.Context) error {
		s, err := a.sessions.ByTokenHash(ctx, tok.Hash())
		if errors.Is(err, domain.ErrSessionNotFound) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("load session: %w", err)
		}

		now := a.now()
		if !s.Revoke(domain.RevokeLogout, now) {
			return nil
		}

		if err := a.sessions.Revoke(ctx, s); err != nil {
			return err
		}

		e, err := domain.NewAuditEntry(now, domain.UserActor(s.UserID(), meta.IP), domain.ActionLogout, domain.ResultOK)
		if err != nil {
			return err
		}

		return a.audit.Append(ctx, e.WithTarget("session", s.ID().String()).WithRequestID(meta.RequestID))
	})
}
