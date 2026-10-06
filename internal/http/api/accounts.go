package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Accounts is the account administration used by the user endpoints.
type Accounts interface {
	User(ctx context.Context, id domain.UserID) (*domain.User, error)
	SetRoles(ctx context.Context, by app.Actor, id domain.UserID, grants []domain.RoleGrant) (app.RolesResult, error)
}

// AccountHandlers serve /roles and /users/….
type AccountHandlers struct {
	sessions Sessions
	accounts Accounts
}

// NewAccountHandlers returns the handlers.
func NewAccountHandlers(s Sessions, a Accounts) AccountHandlers {
	return AccountHandlers{sessions: s, accounts: a}
}

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
