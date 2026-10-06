package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Sessions is the identity HTTP layer used by the auth endpoints.
type Sessions interface {
	Principal(ctx context.Context) domain.Principal
	// CSRFToken returns the request's CSRF token and, for a visitor without
	// a pre-session cookie, the cookie to set.
	CSRFToken(ctx context.Context) (string, *http.Cookie)
	Login(ctx context.Context, login, password string, remember bool) (domain.Principal, string, []*http.Cookie, error)
	Logout(ctx context.Context) (*http.Cookie, error)
}

// AuthHandlers serve /auth/session, /auth/login and /auth/logout.
type AuthHandlers struct {
	sessions Sessions
}

// NewAuthHandlers returns the handlers.
func NewAuthHandlers(s Sessions) AuthHandlers { return AuthHandlers{sessions: s} }

// GetSession implements StrictServerInterface.
func (h AuthHandlers) GetSession(ctx context.Context, _ GetSessionRequestObject) (GetSessionResponseObject, error) {
	token, cookie := h.sessions.CSRFToken(ctx)

	var cookies []*http.Cookie
	if cookie != nil {
		cookies = append(cookies, cookie)
	}

	return sessionResponse{info: sessionInfo(h.sessions.Principal(ctx), token), cookies: cookies}, nil
}

// Login implements StrictServerInterface.
func (h AuthHandlers) Login(ctx context.Context, req LoginRequestObject) (LoginResponseObject, error) {
	remember := req.Body.RememberMe != nil && *req.Body.RememberMe

	p, token, cookies, err := h.sessions.Login(ctx, req.Body.Login, req.Body.Password, remember)

	var rl *domain.RateLimitError
	if errors.As(err, &rl) {
		return rateLimited{err: rl}, nil
	}

	if err != nil {
		return nil, err
	}

	return sessionResponse{info: sessionInfo(p, token), cookies: cookies}, nil
}

// Logout implements StrictServerInterface.
func (h AuthHandlers) Logout(ctx context.Context, _ LogoutRequestObject) (LogoutResponseObject, error) {
	c, err := h.sessions.Logout(ctx)
	if err != nil {
		return nil, err
	}

	return logoutResponse{cookie: c}, nil
}

func sessionInfo(p domain.Principal, token string) SessionInfo {
	info := SessionInfo{
		Authenticated: !p.IsAnonymous(),
		CsrfToken:     token,
		Providers:     []string{domain.ProviderLocal.String()},
		Roles:         []SessionInfoRoles{},
	}

	if p.IsAnonymous() {
		return info
	}

	for _, r := range p.Roles() {
		info.Roles = append(info.Roles, SessionInfoRoles(r.String()))
	}

	u := SessionUser{Id: p.UserID().String(), Username: p.Username().String(), MustChangePassword: p.MustChangePassword()}

	if d := p.DisplayName(); !d.IsZero() {
		name := d.String()
		u.DisplayName = &name
	}

	info.User = &u

	return info
}

// sessionResponse is the session of the caller, with cookies to set.
type sessionResponse struct {
	info    SessionInfo
	cookies []*http.Cookie
}

func (s sessionResponse) write(w http.ResponseWriter) error {
	for _, c := range s.cookies {
		http.SetCookie(w, c)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	return json.NewEncoder(w).Encode(s.info)
}

func (s sessionResponse) VisitGetSessionResponse(w http.ResponseWriter) error { return s.write(w) }
func (s sessionResponse) VisitLoginResponse(w http.ResponseWriter) error      { return s.write(w) }

// rateLimited is a 429 problem with Retry-After.
type rateLimited struct{ err *domain.RateLimitError }

func (r rateLimited) VisitLoginResponse(w http.ResponseWriter) error {
	w.Header().Set("Retry-After", strconv.Itoa(int(r.err.RetryAfter().Seconds())))
	problem.Write(w, problem.FromError(r.err))

	return nil
}

// logoutResponse clears the session cookie.
type logoutResponse struct{ cookie *http.Cookie }

func (l logoutResponse) VisitLogoutResponse(w http.ResponseWriter) error {
	http.SetCookie(w, l.cookie)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)

	return nil
}
