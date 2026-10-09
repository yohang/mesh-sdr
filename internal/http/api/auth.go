package api

import (
	"context"
	"net/http"

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

	return jsonOK{v: sessionInfo(h.sessions.Principal(ctx), token), cookies: cookies}, nil
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

func (j jsonOK) VisitGetSessionResponse(w http.ResponseWriter) error { return j.write(w) }
