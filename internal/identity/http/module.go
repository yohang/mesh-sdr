// Package http is the identity HTTP layer: the router module that resolves
// the client address and the session of every request, enforces CSRF
// protection on state-changing requests (AUTH-019, ADR 0003), authorises by
// role and admin network (AUTH-004, AUTH-016), and serves the login and
// logout actions and page (AUTH-001, AUTH-002, AUTH-005).
package http

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yohang/mesh-sdr/internal/http/clientip"
	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/http/redact"
	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

//go:generate go tool templ generate

// CSRFHeader carries the CSRF token on state-changing requests.
const CSRFHeader = "X-CSRF-Token"

// AuthBodyLimit bounds the request body of the authentication endpoints
// (/login, /logout, /api/v1/auth/*): they take a login and a password.
const AuthBodyLimit = 64 << 10

// Authenticator is the identity application service used by the HTTP layer.
type Authenticator interface {
	Login(ctx context.Context, in app.LoginInput) (app.LoginResult, error)
	Resolve(ctx context.Context, cookie string) (app.Resolution, error)
	// Peek is Resolve without recording activity.
	Peek(ctx context.Context, cookie string) (app.Resolution, error)
	Logout(ctx context.Context, cookie string, meta app.RequestMeta) error
	SessionPolicy() domain.SessionPolicy
}

// PasswordChanger changes the password of the signed-in user.
type PasswordChanger interface {
	Change(ctx context.Context, in app.ChangePasswordInput) (app.ChangePasswordResult, error)
	// MinLength returns the minimum password length in force, shown on the
	// forms.
	MinLength(ctx context.Context) int
}

// Bootstrapper creates the first admin through the one-time setup link
// (AUTH-018).
type Bootstrapper interface {
	Check(token string, meta app.RequestMeta) error
	Complete(ctx context.Context, in app.SetupInput) (app.LoginResult, error)
	MinLength(ctx context.Context) int
}

// ProfileService runs the account page (ACC-004).
type ProfileService interface {
	Me(ctx context.Context, by app.Actor) (*domain.User, error)
	MailEnabled() bool
	SetDisplayName(ctx context.Context, by app.Actor, name string) (*domain.User, error)
	ChangeEmail(ctx context.Context, by app.Actor, email, currentPassword string) (app.EmailChangeResult, error)
	CheckEmailToken(ctx context.Context, token string) error
	ConfirmEmail(ctx context.Context, token string, meta app.RequestMeta) error
}

// AccountService runs session lists and the account administration.
type AccountService interface {
	OwnSessions(ctx context.Context, by app.Actor) ([]app.SessionView, error)
	RevokeOwnSession(ctx context.Context, by app.Actor, ref string) error
	RevokeOtherSessions(ctx context.Context, by app.Actor) (int, error)

	Search(ctx context.Context, q domain.UserQuery) ([]*domain.User, error)
	User(ctx context.Context, id domain.UserID) (*domain.User, error)
	SetRoles(ctx context.Context, by app.Actor, id domain.UserID, grants []domain.RoleGrant) (app.RolesResult, error)
	SetEnabled(ctx context.Context, by app.Actor, id domain.UserID, enabled bool) (bool, error)
	SetGeneratedPassword(ctx context.Context, by app.Actor, id domain.UserID) (string, error)
	UserSessions(ctx context.Context, id domain.UserID) ([]app.SessionView, error)
	RevokeUserSession(ctx context.Context, by app.Actor, id domain.UserID, ref string) error
	RevokeUserSessions(ctx context.Context, by app.Actor, id domain.UserID) (int, error)
	Delete(ctx context.Context, by app.Actor, id domain.UserID) error
	DeleteOwn(ctx context.Context, by app.Actor, currentPassword string) error
	ExportUser(ctx context.Context, by app.Actor, id domain.UserID) (app.Export, error)
	ExportOwn(ctx context.Context, by app.Actor) (app.Export, error)
}

// InvitationService runs invitations (ACC-002).
type InvitationService interface {
	MailEnabled() bool
	DefaultTTL(ctx context.Context) time.Duration
	MinLength(ctx context.Context) int
	Now() time.Time
	Create(ctx context.Context, by app.Actor, in app.CreateInvitationInput) (app.CreatedInvitation, error)
	List(ctx context.Context) ([]*domain.Invitation, error)
	Revoke(ctx context.Context, by app.Actor, id domain.InvitationID) error
	Check(ctx context.Context, token string, meta app.RequestMeta) (*domain.Invitation, error)
	Accept(ctx context.Context, in app.AcceptInput) (app.LoginResult, error)
	TestMail(ctx context.Context, by app.Actor) (domain.Email, error)
}

// ResetService runs password reset by link (ACC-003).
type ResetService interface {
	MailEnabled() bool
	TTL(ctx context.Context) time.Duration
	MinLength(ctx context.Context) int
	Request(ctx context.Context, login string, meta app.RequestMeta) error
	Check(ctx context.Context, token string, meta app.RequestMeta) error
	Confirm(ctx context.Context, token, password string, meta app.RequestMeta) error
	IssueByAdmin(ctx context.Context, by app.Actor, id domain.UserID) (app.AdminResult, error)
}

// AuditService reads the audit log (ACC-010).
type AuditService interface {
	Search(ctx context.Context, f app.AuditFilter) ([]app.AuditRow, int64, error)
	Each(ctx context.Context, f app.AuditFilter, fn func(app.AuditRow) error) error
}

// Services are the application services behind the identity pages.
type Services struct {
	// Keys publishes the token verification keys (JWKS).
	Keys        app.KeySource
	Audit       AuditService
	Resets      ResetService
	Invitations InvitationService
	Auth        Authenticator
	Passwords   PasswordChanger
	Setup       Bootstrapper
	Profile     ProfileService
	Accounts    AccountService
}

// Pages renders HTML pages in the app shell.
type Pages interface {
	// Page writes a page; fragment, when not nil, is written alone for htmx
	// fragment requests.
	Page(w http.ResponseWriter, r *http.Request, status int, title string, content, fragment templ.Component)
	// AdminPage writes a page of the admin area: content is shown in the
	// admin layout, with section (layout.AdminSections) as the current one.
	AdminPage(w http.ResponseWriter, r *http.Request, status int, title, section string, content, fragment templ.Component)
	// Error writes the shell error page for status.
	Error(w http.ResponseWriter, r *http.Request, status int)
}

// Config is the HTTP configuration of the identity module.
type Config struct {
	// HubURL is hub.url: its scheme decides secure cookies (https:
	// __Host- prefix and Secure) and its origin is trusted by the
	// cross-origin protection.
	HubURL string
	// TrustedProxies is http.trusted_proxies.
	TrustedProxies []netip.Prefix
	// AdminNetworks is admin.allowed_networks.
	AdminNetworks []netip.Prefix
}

// Module is the identity router module (internal/http.Module).
type Module struct {
	auth        Authenticator
	passwords   PasswordChanger
	setup       Bootstrapper
	profile     ProfileService
	accounts    AccountService
	invitations InvitationService
	resets      ResetService
	audit       AuditService
	keys        app.KeySource
	now         func() time.Time
	pages       Pages
	logger      *slog.Logger
	resolver    *clientip.Resolver
	cop         *http.CrossOriginProtection
	admin       []netip.Prefix
	secure      bool
	preKey      []byte
	routes      chi.Routes
}

// New returns the module.
func New(svc Services, pages Pages, cfg Config, logger *slog.Logger) (*Module, error) {
	u, err := url.Parse(cfg.HubURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("hub.url %q: not an absolute URL", cfg.HubURL)
	}

	cop := http.NewCrossOriginProtection()
	if err := cop.AddTrustedOrigin(u.Scheme + "://" + u.Host); err != nil {
		return nil, fmt.Errorf("trust hub.url origin: %w", err)
	}

	secure := u.Scheme == "https"
	if !secure {
		logger.Warn("hub.url is not https: session cookies are sent without Secure nor the __Host- prefix; use this only on loopback or a trusted LAN",
			slog.String("hub_url", cfg.HubURL))
	}

	return &Module{
		auth:        svc.Auth,
		passwords:   svc.Passwords,
		setup:       svc.Setup,
		profile:     svc.Profile,
		accounts:    svc.Accounts,
		invitations: svc.Invitations,
		resets:      svc.Resets,
		audit:       svc.Audit,
		keys:        svc.Keys,
		now:         time.Now,
		pages:       pages,
		logger:      logger,
		resolver:    clientip.NewResolver(cfg.TrustedProxies),
		cop:         cop,
		admin:       cfg.AdminNetworks,
		secure:      secure,
		preKey:      []byte(rand.Text()),
	}, nil
}

// Middlewares implements internal/http.Module: client address, session,
// CSRF protection, JSON-only API bodies, then the forced password change.
func (m *Module) Middlewares() []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{m.resolver.Middleware, limitAuthBodies, m.session, m.csrf, m.requireJSON, m.passwordGate}
}

// Routes implements internal/http.Module.
func (m *Module) Routes(r chi.Router) {
	m.routes = r

	// Read-only pages answer GET and HEAD, like the shell's.
	r.Get("/login", m.loginPage)
	r.Head("/login", m.loginPage)
	r.Post("/login", m.loginAction)
	r.Post("/logout", m.logoutAction)

	r.Get(app.SetupPath+"/{token}", m.setupPage)
	r.Head(app.SetupPath+"/{token}", m.setupPage)
	r.Get(app.SetupPath, m.setupLanding)
	r.Head(app.SetupPath, m.setupLanding)
	r.Post(app.SetupPath, m.setupAction)

	listener := r.With(m.Require(domain.RoleListener))
	listener.Get(AccountPath, m.accountPage)
	listener.Head(AccountPath, m.accountPage)
	listener.Post(AccountPath+"/profile", m.profileAction)
	listener.Post(AccountPath+"/email", m.emailAction)
	listener.Post(AccountPath+"/sessions/revoke-others", m.revokeOthersAction)
	listener.Post(AccountPath+"/sessions/{ref}/revoke", m.revokeSessionAction)
	listener.Post(AccountPath+"/export", m.exportOwnAction)
	listener.Post(AccountPath+"/delete", m.deleteOwnAction)

	r.Get(AccountPath+"/email/verify/{token}", m.emailVerifyPage)
	r.Head(AccountPath+"/email/verify/{token}", m.emailVerifyPage)
	r.Get(AccountPath+"/email/verify", m.emailVerifyLanding)
	r.Post(AccountPath+"/email/verify", m.emailVerifyAction)

	admin := r.With(m.Require(domain.RoleAdmin))
	admin.Get(AuditPath, m.auditPage)
	admin.Head(AuditPath, m.auditPage)
	admin.Post(AuditPath+"/export", m.auditExport)
	admin.Get(UsersPath, m.usersPage)
	admin.Head(UsersPath, m.usersPage)
	admin.Get(UsersPath+"/{id}", m.userPage)
	admin.Head(UsersPath+"/{id}", m.userPage)
	admin.Post(UsersPath+"/{id}/roles", m.userRolesAction)
	admin.Post(UsersPath+"/{id}/enable", m.userEnableAction(true))
	admin.Post(UsersPath+"/{id}/disable", m.userEnableAction(false))
	admin.Post(UsersPath+"/{id}/password-reset", m.userResetAction)
	admin.Post(UsersPath+"/{id}/password", m.userPasswordAction)
	admin.Post(UsersPath+"/{id}/sessions/revoke", m.userRevokeAllAction)
	admin.Post(UsersPath+"/{id}/sessions/{ref}/revoke", m.userRevokeAction)
	admin.Post(UsersPath+"/{id}/export", m.userExportAction)
	admin.Post(UsersPath+"/{id}/delete", m.userDeleteAction)
	admin.Get(InvitationsPath, m.invitationsPage)
	admin.Head(InvitationsPath, m.invitationsPage)
	admin.Post(InvitationsPath, m.createInvitationAction)
	admin.Post(InvitationsPath+"/test-mail", m.testMailAction)
	admin.Post(InvitationsPath+"/{id}/revoke", m.revokeInvitationAction)

	r.Get(JWKSPath, m.jwks)
	r.Head(JWKSPath, m.jwks)

	r.Get(ForgotPath, m.forgotPage)
	r.Head(ForgotPath, m.forgotPage)
	r.Post(ForgotPath, m.forgotAction)
	r.Get(ResetPath+"/{token}", m.resetPage)
	r.Head(ResetPath+"/{token}", m.resetPage)
	r.Get(ResetPath, m.resetLanding)
	r.Post(ResetPath, m.resetAction)

	r.Get("/invite/{token}", m.invitePage)
	r.Head("/invite/{token}", m.invitePage)
	r.Get("/invite", m.inviteLanding)
	r.Post("/invite", m.acceptAction)

	r.With(m.Require(domain.RoleListener)).Get(PasswordChangePath, m.passwordPage)
	r.With(m.Require(domain.RoleListener)).Head(PasswordChangePath, m.passwordPage)
	r.With(m.Require(domain.RoleListener)).Post(PasswordChangePath, m.passwordAction)
}

// Cookie names (TECHNICAL_SPEC §5.6). Without TLS (http hub.url, LAN or
// loopback only) the __Host- prefix and Secure are dropped.
func (m *Module) sessionCookieName() string {
	if m.secure {
		return "__Host-rx_session"
	}

	return "rx_session"
}

func (m *Module) presessionCookieName() string {
	if m.secure {
		return "__Host-rx_presession"
	}

	return "rx_presession"
}

func (m *Module) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode,
	}
}

// State is the identity of a request, set by the session middleware.
type State struct {
	principal  domain.Principal
	session    *domain.Session
	token      domain.SessionToken
	presession string
	userAgent  string
}

type stateKey struct{}

// FromContext returns the request identity (anonymous when none).
func FromContext(ctx context.Context) *State {
	if s, ok := ctx.Value(stateKey{}).(*State); ok {
		return s
	}

	return &State{}
}

// WithState returns ctx carrying a principal (tests and non-HTTP callers).
func WithState(ctx context.Context, p domain.Principal) context.Context {
	return context.WithValue(ctx, stateKey{}, &State{principal: p})
}

// Principal returns who makes the request.
func (s *State) Principal() domain.Principal { return s.principal }

// HasSession reports whether the request carries a valid session.
func (s *State) HasSession() bool { return s.session != nil }

// BackgroundHeader marks a request the page made on its own (a live
// fragment refreshed by a hub event, ADR 0016): it does not count as
// activity of the session (ADR 0018).
const BackgroundHeader = "X-Msdr-Background"

// eventsPath is the hub events WebSocket (ADR 0016).
const eventsPath = "/api/ws"

// background reports whether a request is not activity of its user: a
// live fragment refresh (BackgroundHeader), or the upgrade of the hub
// events WebSocket, which a page opens on its own and reopens after every
// network hiccup (ADR 0018).
func background(r *http.Request) bool {
	if r.Header.Get(BackgroundHeader) == "1" {
		return true
	}

	return r.URL.Path == eventsPath && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (m *Module) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := &State{userAgent: r.UserAgent()}

		if c, err := r.Cookie(m.sessionCookieName()); err == nil && c.Value != "" {
			resolve := m.auth.Resolve
			if background(r) {
				resolve = m.auth.Peek
			}

			res, err := resolve(r.Context(), c.Value)

			switch {
			case err == nil:
				st.principal, st.session, st.token = res.Principal, res.Session, res.Token
			case errors.Is(err, domain.ErrUnauthenticated):
				http.SetCookie(w, m.cookie(m.sessionCookieName(), "", -1))
			default:
				// Fail closed: the request continues as anonymous.
				m.logger.ErrorContext(r.Context(), "resolve session", slog.Any("error", err))
			}
		}

		if c, err := r.Cookie(m.presessionCookieName()); err == nil && validPresession(c.Value) {
			st.presession = c.Value
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))
	})
}

func validPresession(v string) bool {
	b, err := base64.RawURLEncoding.DecodeString(v)

	return err == nil && len(b) == 32
}

// presessionToken is the double-submit token bound to a pre-session cookie
// value: HMAC-SHA256 with a per-process key, so a token cannot be computed
// from the cookie alone.
func (m *Module) presessionToken(v string) string {
	mac := hmac.New(sha256.New, m.preKey)
	mac.Write([]byte(v))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// CSRFToken returns the CSRF token of the request: session-bound when it has
// a session, otherwise bound to the pre-session cookie. When the request has
// no pre-session cookie it returns the cookie to set.
func (m *Module) CSRFToken(ctx context.Context) (string, *http.Cookie) {
	st := FromContext(ctx)
	if st.session != nil {
		return st.session.CSRFSecret().Token(st.token), nil
	}

	if st.presession != "" {
		return m.presessionToken(st.presession), nil
	}

	b := make([]byte, 32)
	_, _ = rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)

	return m.presessionToken(v), m.cookie(m.presessionCookieName(), v, 0)
}

func (m *Module) validCSRF(r *http.Request) bool {
	st := FromContext(r.Context())
	got := r.Header.Get(CSRFHeader)

	if got == "" {
		return false
	}

	if st.session != nil {
		return st.session.CSRFSecret().Verify(st.token, got)
	}

	return st.presession != "" && hmac.Equal([]byte(got), []byte(m.presessionToken(st.presession)))
}

func isSafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}

	return false
}

func isAPI(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/") }

// limitAuthBodies caps the body of the authentication endpoints.
func limitAuthBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/login" || p == "/logout" || p == PasswordChangePath || p == app.SetupPath || p == "/invite" || p == ForgotPath || p == ResetPath || strings.HasPrefix(p, "/api/v1/auth/") {
			r.Body = http.MaxBytesReader(w, r.Body, AuthBodyLimit)
		}

		next.ServeHTTP(w, r)
	})
}

// csrf protects every state-changing request: the cross-origin check of
// net/http (Sec-Fetch-Site, Origin vs Host or hub.url), then the CSRF token
// header (ADR 0003 §4, §7).
func (m *Module) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafe(r.Method) {
			next.ServeHTTP(w, r)

			return
		}

		reason := ""

		switch {
		case m.cop.Check(r) != nil:
			reason = "cross_origin"
		case !m.validCSRF(r):
			reason = "token"
		}

		if reason == "" {
			next.ServeHTTP(w, r)

			return
		}

		m.logger.LogAttrs(r.Context(), slog.LevelWarn, "csrf check failed",
			slog.String("reason", reason), slog.String("method", r.Method), slog.String("path", redact.Path(r.URL.Path)),
			slog.String("origin", r.Header.Get("Origin")), slog.String("sec_fetch_site", r.Header.Get("Sec-Fetch-Site")),
			slog.String("request_id", middleware.GetReqID(r.Context())))

		m.deny(w, r, http.StatusForbidden, domain.ErrCSRF)
	})
}

// requireJSON answers 415 to state-changing /api/v1 requests with a body
// that is not application/json (ADR 0003 §6).
func (m *Module) requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafe(r.Method) || !isAPI(r) || (r.ContentLength == 0 && len(r.TransferEncoding) == 0) {
			next.ServeHTTP(w, r)

			return
		}

		ct := r.Header.Get("Content-Type")
		mt, _, _ := strings.Cut(ct, ";")
		mt = strings.ToLower(strings.TrimSpace(mt))

		if mt == "application/json" {
			next.ServeHTTP(w, r)

			return
		}

		problem.Write(w, problem.New(http.StatusUnsupportedMediaType, "unsupported_media_type", "the request body must be application/json"))
	})
}

// Authorize checks that the request principal holds role, and for admin
// operations that the client address is in admin.allowed_networks. It
// returns ErrUnauthenticated, ErrForbidden or ErrAdminNetworkDenied.
func (m *Module) Authorize(ctx context.Context, role domain.Role) error {
	if role == domain.RoleAnonymous {
		return nil
	}

	p := FromContext(ctx).Principal()

	switch {
	case p.IsAnonymous():
		return domain.ErrUnauthenticated
	case !p.Has(role):
		return domain.ErrForbidden
	case role == domain.RoleAdmin && !clientip.In(clientip.From(ctx), m.admin):
		return domain.ErrAdminNetworkDenied
	}

	return nil
}

// Require guards HTML routes: anonymous visitors are redirected to the login
// page with a safe next, forbidden requests get the shell 403 page.
func (m *Module) Require(role domain.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			err := m.Authorize(r.Context(), role)

			switch {
			case err == nil:
				next.ServeHTTP(w, r)
			case errors.Is(err, domain.ErrUnauthenticated):
				m.redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()))
			default:
				m.logger.WarnContext(r.Context(), "access denied", slog.String("path", redact.Path(r.URL.Path)), slog.Any("error", err))
				m.pages.Error(w, r, http.StatusForbidden)
			}
		})
	}
}

// redirect sends a full-page redirect, for htmx (HX-Redirect) and plain
// requests (303).
func (m *Module) redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)

		return
	}

	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (m *Module) deny(w http.ResponseWriter, r *http.Request, status int, err *shared.Error) {
	if isAPI(r) {
		problem.Write(w, problem.New(status, string(err.Code()), err.Message()))

		return
	}

	m.pages.Error(w, r, status)
}

func (m *Module) meta(ctx context.Context) app.RequestMeta {
	return app.RequestMeta{
		IP:        clientip.From(ctx),
		UserAgent: FromContext(ctx).userAgent,
		RequestID: middleware.GetReqID(ctx),
	}
}

// Actor returns who makes the request, with its request metadata.
func (m *Module) Actor(ctx context.Context) app.Actor {
	return app.Actor{Principal: FromContext(ctx).Principal(), Meta: m.meta(ctx)}
}

// Principal returns who makes the request.
func (m *Module) Principal(ctx context.Context) domain.Principal { return FromContext(ctx).Principal() }

// Login opens a session for the request (login page). It returns the new
// principal, the CSRF token of the new session and the cookies to set.
func (m *Module) Login(ctx context.Context, login, password string, remember bool) (domain.Principal, string, []*http.Cookie, error) {
	st := FromContext(ctx)

	previous := ""
	if st.session != nil {
		previous = st.token.Cookie()
	}

	res, err := m.auth.Login(ctx, app.LoginInput{Login: login, Password: password, Remember: remember, Previous: previous, Meta: m.meta(ctx)})
	if err != nil {
		return domain.Principal{}, "", nil, err
	}

	return res.Principal, res.Session.CSRFSecret().Token(res.Token), m.sessionCookies(res), nil
}

// sessionCookies are the cookies of a new session: the session cookie
// (persistent with "remember me") and the cleared pre-session cookie.
func (m *Module) sessionCookies(res app.LoginResult) []*http.Cookie {
	maxAge := 0
	if res.Remember {
		maxAge = int(res.Session.AbsoluteExpiresAt().Sub(res.Session.CreatedAt()).Seconds())
	}

	return []*http.Cookie{
		m.cookie(m.sessionCookieName(), res.Token.Cookie(), maxAge),
		m.cookie(m.presessionCookieName(), "", -1),
	}
}

// ChangePassword changes the password of the request's user, and returns
// the principal, the CSRF token of the new session (the request's session
// is replaced) and the cookie to set. forced tells whether the change was
// required.
func (m *Module) ChangePassword(ctx context.Context, current, newPassword string) (p domain.Principal, csrf string, cookie *http.Cookie, forced bool, err error) {
	st := FromContext(ctx)
	if st.session == nil {
		return domain.Principal{}, "", nil, false, domain.ErrUnauthenticated
	}

	res, err := m.passwords.Change(ctx, app.ChangePasswordInput{Session: st.session, Current: current, New: newPassword, Meta: m.meta(ctx)})
	if err != nil {
		return domain.Principal{}, "", nil, false, err
	}

	maxAge := 0
	if res.Remember {
		maxAge = max(1, int(res.Session.AbsoluteExpiresAt().Sub(res.Session.CreatedAt()).Seconds()))
	}

	return res.Principal, res.Session.CSRFSecret().Token(res.Token), m.cookie(m.sessionCookieName(), res.Token.Cookie(), maxAge), res.Forced, nil
}

// CheckSession re-reads the session of a long-lived request (the hub
// events WebSocket) without recording activity. It returns when the
// session ends at the latest if nothing else happens (its idle or absolute
// expiry), domain.ErrUnauthenticated once it has ended, and a zero time for
// a request without a session.
func (m *Module) CheckSession(ctx context.Context, r *http.Request) (time.Time, error) {
	if FromContext(ctx).session == nil {
		return time.Time{}, nil
	}

	c, err := r.Cookie(m.sessionCookieName())
	if err != nil || c.Value == "" {
		return time.Time{}, domain.ErrUnauthenticated
	}

	res, err := m.auth.Peek(ctx, c.Value)
	if err != nil {
		return time.Time{}, err
	}

	if res.Session.ID() != FromContext(ctx).session.ID() {
		return time.Time{}, domain.ErrUnauthenticated
	}

	until := res.Session.AbsoluteExpiresAt()
	if idle := res.Session.IdleExpiresAt(); idle.Before(until) {
		until = idle
	}

	return until, nil
}

// SessionRef returns the public handle of the request's session ("" when
// anonymous): the sid claim of its access tokens.
func (m *Module) SessionRef(ctx context.Context) string {
	if st := FromContext(ctx); st.session != nil {
		return st.session.Ref()
	}

	return ""
}

// Logout ends the request's session and returns the cookie to clear it.
func (m *Module) Logout(ctx context.Context) (*http.Cookie, error) {
	st := FromContext(ctx)
	if st.session != nil {
		if err := m.auth.Logout(ctx, st.token.Cookie(), m.meta(ctx)); err != nil {
			return nil, err
		}
	}

	return m.cookie(m.sessionCookieName(), "", -1), nil
}
