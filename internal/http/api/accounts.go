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
	Search(ctx context.Context, q domain.UserQuery) ([]*domain.User, error)
	SetEnabled(ctx context.Context, by app.Actor, id domain.UserID, enabled bool) (bool, error)
	SetDisplayName(ctx context.Context, by app.Actor, id domain.UserID, name string) error
	SetGeneratedPassword(ctx context.Context, by app.Actor, id domain.UserID) (string, error)
	Delete(ctx context.Context, by app.Actor, id domain.UserID) error
	DeleteOwn(ctx context.Context, by app.Actor, currentPassword string) error
	ExportUser(ctx context.Context, by app.Actor, id domain.UserID) (app.Export, error)
	ExportOwn(ctx context.Context, by app.Actor) (app.Export, error)
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

func user(u *domain.User) User {
	return User{
		Id: u.ID().String(), Username: u.Username().String(), DisplayName: optString(u.DisplayName().String()),
		Email: optString(u.Email().String()), EmailVerified: !u.EmailVerifiedAt().IsZero(),
		Roles: strings.Split(app.GrantNames(u.Grants()), ","), Enabled: u.Enabled(), MustChangePassword: u.MustChangePassword(),
		CreatedAt: u.CreatedAt(), LastLoginAt: optTime(u.LastLoginAt()),
	}
}

// ListUsers implements StrictServerInterface.
func (h AccountHandlers) ListUsers(ctx context.Context, req ListUsersRequestObject) (ListUsersResponseObject, error) {
	q := domain.UserQuery{Limit: 50}
	p := req.Params

	if p.Q != nil {
		q.Text = *p.Q
	}

	if p.Role != nil {
		r, err := domain.ParseRole(string(*p.Role))
		if err != nil {
			return nil, err
		}

		q.Role = r
	}

	q.Enabled = p.Enabled

	if p.NeverSignedIn != nil {
		q.NeverLoggedIn = *p.NeverSignedIn
	}

	if p.After != nil {
		q.After = *p.After
	}

	if p.Limit != nil {
		q.Limit = *p.Limit
	}

	users, err := h.accounts.Search(ctx, q)
	if err != nil {
		return nil, err
	}

	out := UserList{Users: make([]User, 0, len(users))}
	for _, u := range users {
		out.Users = append(out.Users, user(u))
	}

	if len(users) == q.Limit {
		out.NextAfter = optString(users[len(users)-1].Username().Key())
	}

	return jsonOK{out}, nil
}

// GetUser implements StrictServerInterface.
func (h AccountHandlers) GetUser(ctx context.Context, req GetUserRequestObject) (GetUserResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	u, err := h.accounts.User(ctx, id)
	if err != nil {
		return nil, err
	}

	return jsonOK{user(u)}, nil
}

// UpdateUser implements StrictServerInterface.
func (h AccountHandlers) UpdateUser(ctx context.Context, req UpdateUserRequestObject) (UpdateUserResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	by := h.sessions.Actor(ctx)

	if req.Body.DisplayName != nil {
		if err := h.accounts.SetDisplayName(ctx, by, id, *req.Body.DisplayName); err != nil {
			return nil, err
		}
	}

	if req.Body.Enabled != nil {
		if _, err := h.accounts.SetEnabled(ctx, by, id, *req.Body.Enabled); err != nil {
			return nil, err
		}
	}

	u, err := h.accounts.User(ctx, id)
	if err != nil {
		return nil, err
	}

	return jsonOK{user(u)}, nil
}

// SetGeneratedPassword implements StrictServerInterface.
func (h AccountHandlers) SetGeneratedPassword(ctx context.Context, req SetGeneratedPasswordRequestObject) (SetGeneratedPasswordResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	pw, err := h.accounts.SetGeneratedPassword(ctx, h.sessions.Actor(ctx), id)
	if err != nil {
		return nil, err
	}

	return jsonOK{GeneratedPassword{Password: pw}}, nil
}

func (j jsonOK) VisitListUsersResponse(w http.ResponseWriter) error            { return j.write(w) }
func (j jsonOK) VisitGetUserResponse(w http.ResponseWriter) error              { return j.write(w) }
func (j jsonOK) VisitUpdateUserResponse(w http.ResponseWriter) error           { return j.write(w) }
func (j jsonOK) VisitSetGeneratedPasswordResponse(w http.ResponseWriter) error { return j.write(w) }

// ExportMe implements StrictServerInterface.
func (h AccountHandlers) ExportMe(ctx context.Context, _ ExportMeRequestObject) (ExportMeResponseObject, error) {
	e, err := h.accounts.ExportOwn(ctx, h.sessions.Actor(ctx))
	if err != nil {
		return nil, err
	}

	return jsonOK{e}, nil
}

// ExportUser implements StrictServerInterface.
func (h AccountHandlers) ExportUser(ctx context.Context, req ExportUserRequestObject) (ExportUserResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	e, err := h.accounts.ExportUser(ctx, h.sessions.Actor(ctx), id)
	if err != nil {
		return nil, err
	}

	return jsonOK{e}, nil
}

// DeleteMe implements StrictServerInterface.
func (h AccountHandlers) DeleteMe(ctx context.Context, req DeleteMeRequestObject) (DeleteMeResponseObject, error) {
	err := h.accounts.DeleteOwn(ctx, h.sessions.Actor(ctx), req.Body.CurrentPassword)

	var rl *domain.RateLimitError
	if errors.As(err, &rl) {
		return rateLimited{err: rl}, nil
	}

	if err != nil {
		return nil, err
	}

	c, err := h.sessions.Logout(ctx)
	if err != nil {
		return nil, err
	}

	return logoutResponse{cookie: c}, nil
}

// DeleteUser implements StrictServerInterface.
func (h AccountHandlers) DeleteUser(ctx context.Context, req DeleteUserRequestObject) (DeleteUserResponseObject, error) {
	id, err := parseUserID(req.Id)
	if err != nil {
		return nil, err
	}

	if err := h.accounts.Delete(ctx, h.sessions.Actor(ctx), id); err != nil {
		return nil, err
	}

	return noContent{}, nil
}

func (j jsonOK) VisitExportMeResponse(w http.ResponseWriter) error   { return j.write(w) }
func (j jsonOK) VisitExportUserResponse(w http.ResponseWriter) error { return j.write(w) }

func (n noContent) VisitDeleteUserResponse(w http.ResponseWriter) error { return n.write(w) }

func (r rateLimited) VisitDeleteMeResponse(w http.ResponseWriter) error { return r.write(w) }
