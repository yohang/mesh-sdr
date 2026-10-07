package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Sessions is the identity HTTP layer used by the session and token
// endpoints.
type Sessions interface {
	Principal(ctx context.Context) domain.Principal
	// CSRFToken returns the request's CSRF token and, for a visitor without
	// a pre-session cookie, the cookie to set.
	CSRFToken(ctx context.Context) (string, *http.Cookie)
	// Actor returns who makes the request.
	Actor(ctx context.Context) app.Actor
}

// AuthHandlers serve /auth/session: the CSRF token of the scripts.
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

func (s sessionResponse) VisitGetSessionResponse(w http.ResponseWriter) error {
	for _, c := range s.cookies {
		http.SetCookie(w, c)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	return json.NewEncoder(w).Encode(s.info)
}

// rateLimited is a 429 problem with Retry-After.
type rateLimited struct{ err *domain.RateLimitError }

func (r rateLimited) write(w http.ResponseWriter) error {
	w.Header().Set("Retry-After", strconv.Itoa(int(r.err.RetryAfter().Seconds())))
	problem.Write(w, problem.FromError(r.err))

	return nil
}

// jsonOK writes a 200 JSON body, not cached.
type jsonOK struct{ v any }

func (j jsonOK) write(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	return json.NewEncoder(w).Encode(j.v)
}

func optString(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}
