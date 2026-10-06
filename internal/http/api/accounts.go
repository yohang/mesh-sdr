package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Accounts is the account administration used by the user endpoints.
type Accounts interface {
	User(ctx context.Context, id domain.UserID) (*domain.User, error)
	SetRoles(ctx context.Context, by app.Actor, id domain.UserID, grants []domain.RoleGrant) (app.RolesResult, error)
	OwnSessions(ctx context.Context, by app.Actor) ([]app.SessionView, error)
	UserSessions(ctx context.Context, id domain.UserID) ([]app.SessionView, error)
	RevokeOwnSession(ctx context.Context, by app.Actor, ref string) error
	RevokeAllOwnSessions(ctx context.Context, by app.Actor) (int, error)
	RevokeUserSession(ctx context.Context, by app.Actor, id domain.UserID, ref string) error
	RevokeUserSessions(ctx context.Context, by app.Actor, id domain.UserID) (int, error)
}

// Profile is the account page service used by the /me endpoints.
type Profile interface {
	Me(ctx context.Context, by app.Actor) (*domain.User, error)
	SetDisplayName(ctx context.Context, by app.Actor, name string) (*domain.User, error)
	ChangeEmail(ctx context.Context, by app.Actor, email, currentPassword string) (app.EmailChangeResult, error)
}

// AccountHandlers serve /me, /roles and /users/….
type AccountHandlers struct {
	sessions Sessions
	accounts Accounts
	profile  Profile
}

// NewAccountHandlers returns the handlers.
func NewAccountHandlers(s Sessions, a Accounts, p Profile) AccountHandlers {
	return AccountHandlers{sessions: s, accounts: a, profile: p}
}

func me(u *domain.User) Me {
	out := Me{
		Id: u.ID().String(), Username: u.Username().String(), EmailVerified: !u.EmailVerifiedAt().IsZero(),
		Roles: strings.Split(app.GrantNames(u.Grants()), ","), MustChangePassword: u.MustChangePassword(),
		Identities: []string{},
	}

	if d := u.DisplayName(); !d.IsZero() {
		s := d.String()
		out.DisplayName = &s
	}

	if e := u.Email(); !e.IsZero() {
		s := e.String()
		out.Email = &s
	}

	for _, i := range u.Identities() {
		out.Identities = append(out.Identities, i.Provider().String())
	}

	return out
}

// GetMe implements StrictServerInterface.
func (h AccountHandlers) GetMe(ctx context.Context, _ GetMeRequestObject) (GetMeResponseObject, error) {
	u, err := h.profile.Me(ctx, h.sessions.Actor(ctx))
	if err != nil {
		return nil, err
	}

	return jsonOK{me(u)}, nil
}

// UpdateMe implements StrictServerInterface.
func (h AccountHandlers) UpdateMe(ctx context.Context, req UpdateMeRequestObject) (UpdateMeResponseObject, error) {
	u, err := h.profile.SetDisplayName(ctx, h.sessions.Actor(ctx), req.Body.DisplayName)
	if err != nil {
		return nil, err
	}

	return jsonOK{me(u)}, nil
}

// ChangeMyEmail implements StrictServerInterface.
func (h AccountHandlers) ChangeMyEmail(ctx context.Context, req ChangeMyEmailRequestObject) (ChangeMyEmailResponseObject, error) {
	res, err := h.profile.ChangeEmail(ctx, h.sessions.Actor(ctx), req.Body.Email, req.Body.CurrentPassword)

	var rl *domain.RateLimitError
	if errors.As(err, &rl) {
		return rateLimited{err: rl}, nil
	}

	if err != nil {
		return nil, err
	}

	return jsonOK{EmailChangeResult{Pending: res.Pending}}, nil
}

func (j jsonOK) VisitGetMeResponse(w http.ResponseWriter) error         { return j.write(w) }
func (j jsonOK) VisitUpdateMeResponse(w http.ResponseWriter) error      { return j.write(w) }
func (j jsonOK) VisitChangeMyEmailResponse(w http.ResponseWriter) error { return j.write(w) }

// parseUserID maps an invalid id to user_not_found: ids are opaque.
func parseUserID(s string) (domain.UserID, error) {
	id, err := domain.ParseUserID(s)
	if err != nil {
		return domain.UserID{}, domain.ErrUserNotFound
	}

	return id, nil
}

// ListRoles implements StrictServerInterface.
func (AccountHandlers) ListRoles(context.Context, ListRolesRequestObject) (ListRolesResponseObject, error) {
	roles := []RoleInfo{
		{Name: RoleInfoNameListener, Rank: int(domain.RoleListener), Grantable: false},
		{Name: RoleInfoNameOperator, Rank: int(domain.RoleOperator), Grantable: true},
		{Name: RoleInfoNameAdmin, Rank: int(domain.RoleAdmin), Grantable: true},
	}

	return jsonOK{RoleList{Roles: roles}}, nil
}

// GetUserRoles implements StrictServerInterface.
func (h AccountHandlers) GetUserRoles(ctx context.Context, req GetUserRolesRequestObject) (GetUserRolesResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	u, err := h.accounts.User(ctx, id)
	if err != nil {
		return nil, err
	}

	return jsonOK{roleGrants(u.Grants())}, nil
}

// SetUserRoles implements StrictServerInterface.
func (h AccountHandlers) SetUserRoles(ctx context.Context, req SetUserRolesRequestObject) (SetUserRolesResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	grants := make([]domain.RoleGrant, 0, len(req.Body.Grants))

	for _, g := range req.Body.Grants {
		role, err := domain.ParseRole(string(g.Role))
		if err != nil {
			return nil, err
		}

		var dev domain.DeviceID
		if g.DeviceId != nil && *g.DeviceId != "" {
			if dev, err = domain.NewDeviceID(*g.DeviceId); err != nil {
				return nil, err
			}
		}

		grant, err := domain.NewRoleGrant(role, dev)
		if err != nil {
			return nil, err
		}

		grants = append(grants, grant)
	}

	res, err := h.accounts.SetRoles(ctx, h.sessions.Actor(ctx), id, grants)
	if err != nil {
		return nil, err
	}

	return jsonOK{roleGrants(res.User.Grants())}, nil
}

func roleGrants(grants []domain.RoleGrant) RoleGrants {
	out := RoleGrants{Grants: make([]RoleGrant, 0, len(grants))}

	for _, g := range grants {
		rg := RoleGrant{Role: RoleGrantRole(g.Role().String())}
		if !g.Global() {
			d := g.Device().String()
			rg.DeviceId = &d
		}

		out.Grants = append(out.Grants, rg)
	}

	return out
}

// jsonOK writes a 200 JSON body, not cached.
type jsonOK struct{ v any }

func (j jsonOK) write(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	return json.NewEncoder(w).Encode(j.v)
}

func (j jsonOK) VisitListRolesResponse(w http.ResponseWriter) error    { return j.write(w) }
func (j jsonOK) VisitGetUserRolesResponse(w http.ResponseWriter) error { return j.write(w) }
func (j jsonOK) VisitSetUserRolesResponse(w http.ResponseWriter) error { return j.write(w) }

func sessionList(views []app.SessionView) SessionList {
	out := SessionList{Sessions: make([]SessionView, 0, len(views))}

	for _, v := range views {
		sv := SessionView{
			Id: v.Ref, Current: v.Current, CreatedAt: v.CreatedAt, LastSeenAt: v.LastSeenAt, ExpiresAt: v.ExpiresAt,
			Browser: v.Browser, System: v.System,
		}

		if v.IP != "" {
			ip := v.IP
			sv.Ip = &ip
		}

		if v.UserAgent != "" {
			ua := v.UserAgent
			sv.UserAgent = &ua
		}

		out.Sessions = append(out.Sessions, sv)
	}

	return out
}

// ListOwnSessions implements StrictServerInterface.
func (h AccountHandlers) ListOwnSessions(ctx context.Context, _ ListOwnSessionsRequestObject) (ListOwnSessionsResponseObject, error) {
	views, err := h.accounts.OwnSessions(ctx, h.sessions.Actor(ctx))
	if err != nil {
		return nil, err
	}

	return jsonOK{sessionList(views)}, nil
}

// RevokeOwnSession implements StrictServerInterface.
func (h AccountHandlers) RevokeOwnSession(ctx context.Context, req RevokeOwnSessionRequestObject) (RevokeOwnSessionResponseObject, error) {
	if err := h.accounts.RevokeOwnSession(ctx, h.sessions.Actor(ctx), req.Ref); err != nil {
		return nil, err
	}

	return noContent{}, nil
}

// LogoutAll implements StrictServerInterface.
func (h AccountHandlers) LogoutAll(ctx context.Context, _ LogoutAllRequestObject) (LogoutAllResponseObject, error) {
	if _, err := h.accounts.RevokeAllOwnSessions(ctx, h.sessions.Actor(ctx)); err != nil {
		return nil, err
	}

	c, err := h.sessions.Logout(ctx)
	if err != nil {
		return nil, err
	}

	return logoutResponse{cookie: c}, nil
}

// ListUserSessions implements StrictServerInterface.
func (h AccountHandlers) ListUserSessions(ctx context.Context, req ListUserSessionsRequestObject) (ListUserSessionsResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	views, err := h.accounts.UserSessions(ctx, id)
	if err != nil {
		return nil, err
	}

	return jsonOK{sessionList(views)}, nil
}

// RevokeUserSession implements StrictServerInterface.
func (h AccountHandlers) RevokeUserSession(ctx context.Context, req RevokeUserSessionRequestObject) (RevokeUserSessionResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	if err := h.accounts.RevokeUserSession(ctx, h.sessions.Actor(ctx), id, req.Ref); err != nil {
		return nil, err
	}

	return noContent{}, nil
}

// RevokeUserSessions implements StrictServerInterface.
func (h AccountHandlers) RevokeUserSessions(ctx context.Context, req RevokeUserSessionsRequestObject) (RevokeUserSessionsResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	n, err := h.accounts.RevokeUserSessions(ctx, h.sessions.Actor(ctx), id)
	if err != nil {
		return nil, err
	}

	return jsonOK{Revoked{Revoked: n}}, nil
}

func (j jsonOK) VisitListOwnSessionsResponse(w http.ResponseWriter) error    { return j.write(w) }
func (j jsonOK) VisitListUserSessionsResponse(w http.ResponseWriter) error   { return j.write(w) }
func (j jsonOK) VisitRevokeUserSessionsResponse(w http.ResponseWriter) error { return j.write(w) }

// noContent is an empty 204.
type noContent struct{}

func (noContent) write(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)

	return nil
}

func (n noContent) VisitRevokeOwnSessionResponse(w http.ResponseWriter) error  { return n.write(w) }
func (n noContent) VisitRevokeUserSessionResponse(w http.ResponseWriter) error { return n.write(w) }
