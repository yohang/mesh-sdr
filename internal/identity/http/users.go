package http

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// UsersPath is Admin › Users (ACC-008).
const UsersPath = "/admin/users"

const (
	pageTitleUsers = "Users"
	usersPageSize  = 50
)

// usersView is the user list with its filters.
type usersView struct {
	Query  usersQuery
	Users  []*domain.User
	NextQS string
}

// usersQuery is the filter form as typed.
type usersQuery struct {
	Text  string
	Role  string
	State string
	Never bool
	After string
}

func (q usersQuery) domain() domain.UserQuery {
	out := domain.UserQuery{Text: strings.TrimSpace(q.Text), NeverLoggedIn: q.Never, After: q.After, Limit: usersPageSize}

	if r, err := domain.ParseRole(q.Role); err == nil {
		out.Role = r
	}

	switch q.State {
	case "enabled":
		on := true
		out.Enabled = &on
	case "disabled":
		off := false
		out.Enabled = &off
	}

	return out
}

func (m *Module) usersPage(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	v := r.URL.Query()
	q := usersQuery{Text: v.Get("q"), Role: v.Get("role"), State: v.Get("state"), Never: v.Get("never") == "1", After: v.Get("after")}

	users, err := m.accounts.Search(r.Context(), q.domain())
	if err != nil {
		m.pageFailed(w, r, err)

		return
	}

	view := usersView{Query: q, Users: users}

	if len(users) == usersPageSize {
		next := url.Values{"q": {q.Text}, "role": {q.Role}, "state": {q.State}, "after": {users[len(users)-1].Username().Key()}}
		if q.Never {
			next.Set("never", "1")
		}

		view.NextQS = next.Encode()
	}

	m.pages.AdminPage(w, r, http.StatusOK, pageTitleUsers, "users", usersPage(view), nil)
}

// userView is the detail page of a user.
type userView struct {
	User        *domain.User
	Sessions    []app.SessionView
	MailEnabled bool
	Self        bool
	Notice      notice
	// Password is a generated password, shown once.
	Password string
	// ResetLink is an admin-issued reset link to copy, shown once.
	ResetLink string
	// Roles is the role form: the global role and the devices of
	// device-scoped operator grants.
	Role    string
	Devices string
}

func roleForm(u *domain.User) (string, string) {
	role := u.Role().String()

	var devices []string

	for _, g := range u.Grants() {
		if !g.Global() {
			devices = append(devices, g.Device().String())
		}
	}

	return role, strings.Join(devices, ", ")
}

// userView loads a user's detail page; it answers 404 itself.
func (m *Module) userView(w http.ResponseWriter, r *http.Request) (userView, bool) {
	id, err := domain.ParseUserID(chi.URLParam(r, "id"))
	if err != nil {
		m.pages.Error(w, r, http.StatusNotFound)

		return userView{}, false
	}

	u, err := m.accounts.User(r.Context(), id)
	if errors.Is(err, domain.ErrUserNotFound) {
		m.pages.Error(w, r, http.StatusNotFound)

		return userView{}, false
	}

	if err != nil {
		m.pageFailed(w, r, err)

		return userView{}, false
	}

	sessions, err := m.accounts.UserSessions(r.Context(), id)
	if err != nil {
		m.pageFailed(w, r, err)

		return userView{}, false
	}

	role, devices := roleForm(u)

	return userView{
		User: u, Sessions: sessions, MailEnabled: m.resets.MailEnabled(), Role: role, Devices: devices,
		Self: m.Principal(r.Context()).UserID() == id,
	}, true
}

func (m *Module) userPage(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	if v, ok := m.userView(w, r); ok {
		m.pages.AdminPage(w, r, http.StatusOK, v.User.Username().String(), "users", userPage(v), nil)
	}
}

// userAction runs an admin action on the user of the path, then shows its
// page again with the outcome.
func (m *Module) userAction(w http.ResponseWriter, r *http.Request, run func(id domain.UserID, v *userView) (string, error)) {
	noIndex(w)

	if !m.parseForm(w, r) {
		return
	}

	before, ok := m.userView(w, r)
	if !ok {
		return
	}

	msg, err := run(before.User.ID(), &before)

	v, ok := m.userView(w, r)
	if !ok {
		return
	}

	v.Password, v.ResetLink = before.Password, before.ResetLink
	status := http.StatusOK

	if err != nil {
		v.Notice, status = m.formError(r, err, "")
		v.Role, v.Devices = before.Role, before.Devices
	} else {
		v.Notice = notice{Text: msg}
	}

	m.pages.AdminPage(w, r, status, v.User.Username().String(), "users", userPage(v), nil)
}

// parseGrants reads the role form: a global role, plus device-scoped
// operator grants for a listener.
func parseGrants(role, devices string) ([]domain.RoleGrant, error) {
	r, err := domain.ParseRole(role)
	if err != nil || r == domain.RoleAnonymous {
		return nil, domain.ErrInvalidRole
	}

	if r != domain.RoleListener {
		g, err := domain.NewRoleGrant(r, domain.DeviceID{})

		return []domain.RoleGrant{g}, err
	}

	var grants []domain.RoleGrant

	for _, d := range strings.FieldsFunc(devices, func(c rune) bool { return c == ',' || c == ' ' }) {
		dev, err := domain.NewDeviceID(d)
		if err != nil {
			return nil, err
		}

		g, err := domain.NewRoleGrant(domain.RoleOperator, dev)
		if err != nil {
			return nil, err
		}

		grants = append(grants, g)
	}

	return grants, nil
}

func (m *Module) userRolesAction(w http.ResponseWriter, r *http.Request) {
	m.userAction(w, r, func(id domain.UserID, v *userView) (string, error) {
		v.Role, v.Devices = r.PostForm.Get("role"), r.PostForm.Get("devices")

		grants, err := parseGrants(v.Role, v.Devices)
		if err != nil {
			return "", err
		}

		res, err := m.accounts.SetRoles(r.Context(), m.Actor(r.Context()), id, grants)
		if err != nil {
			return "", err
		}

		if !res.Changed {
			return "The roles did not change.", nil
		}

		return "The roles were changed; the user's sessions were signed out.", nil
	})
}

func (m *Module) userEnableAction(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.userAction(w, r, func(id domain.UserID, _ *userView) (string, error) {
			if _, err := m.accounts.SetEnabled(r.Context(), m.Actor(r.Context()), id, enabled); err != nil {
				return "", err
			}

			if enabled {
				return "The account is enabled.", nil
			}

			return "The account is disabled; its sessions were signed out.", nil
		})
	}
}

func (m *Module) userResetAction(w http.ResponseWriter, r *http.Request) {
	m.userAction(w, r, func(id domain.UserID, v *userView) (string, error) {
		res, err := m.resets.IssueByAdmin(r.Context(), m.Actor(r.Context()), id)
		if err != nil {
			return "", err
		}

		if res.Mailed {
			return "A password reset link was e-mailed to the user.", nil
		}

		v.ResetLink = res.Link

		return "Copy the reset link below and send it to the user: it is shown only once.", nil
	})
}

func (m *Module) userPasswordAction(w http.ResponseWriter, r *http.Request) {
	m.userAction(w, r, func(id domain.UserID, v *userView) (string, error) {
		pw, err := m.accounts.SetGeneratedPassword(r.Context(), m.Actor(r.Context()), id)
		if err != nil {
			return "", err
		}

		v.Password = pw

		return "A generated password was set; the user must change it at the next sign-in. It is shown only once.", nil
	})
}

func (m *Module) userRevokeAllAction(w http.ResponseWriter, r *http.Request) {
	m.userAction(w, r, func(id domain.UserID, _ *userView) (string, error) {
		_, err := m.accounts.RevokeUserSessions(r.Context(), m.Actor(r.Context()), id)

		return "Every session of the user was signed out.", err
	})
}

func (m *Module) userRevokeAction(w http.ResponseWriter, r *http.Request) {
	m.userAction(w, r, func(id domain.UserID, _ *userView) (string, error) {
		err := m.accounts.RevokeUserSession(r.Context(), m.Actor(r.Context()), id, chi.URLParam(r, "ref"))

		return "The session was signed out.", err
	})
}
