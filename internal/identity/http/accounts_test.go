package http_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
)

// signedIn returns a client signed in as name (password `password`).
func (h *hub) signedIn(name string) *client {
	h.t.Helper()

	c := h.client()
	c.session()

	if res := c.login(name, password, false); res.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("login %s = %d", name, res.StatusCode)
	}

	c.session()

	return c
}

func (h *hub) userID(name string) string {
	h.t.Helper()

	users, err := h.admin.List(context.Background(), true)
	if err != nil {
		h.t.Fatal(err)
	}

	for _, u := range users {
		if u.Username().String() == name {
			return u.ID().String()
		}
	}

	h.t.Fatalf("no user %s", name)

	return ""
}

func (c *client) api(method, path, body string) *http.Response {
	c.h.t.Helper()

	ctype := ""
	if body != "" {
		ctype = "application/json"
	}

	return c.do(method, path, ctype, body, map[string]string{identityhttp.CSRFHeader: c.token})
}

// TestRolesForm covers the refusals of the roles form of Admin › Users
// (its happy path is in TestUsersPages), and its rights.
func TestRolesForm(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)
	h.addUser("alice", domain.RoleListener)

	alice := "/admin/users/" + h.userID("alice") + "/roles"
	root := h.signedIn("root")

	for _, tc := range []struct {
		path  string
		form  url.Values
		want  string
		state int
	}{
		{alice, url.Values{"role": {"root"}}, "role", http.StatusUnprocessableEntity},
		{alice, url.Values{"role": {"listener"}, "devices": {"Bad Device"}}, "device", http.StatusUnprocessableEntity},
		{"/admin/users/not-an-id/roles", url.Values{"role": {"listener"}}, "", http.StatusNotFound},
		{"/admin/users/" + h.userID("root") + "/roles", url.Values{"role": {"listener"}}, "last enabled admin", http.StatusUnprocessableEntity},
	} {
		res := root.form(tc.path, tc.form, false)
		if b := body(t, res); res.StatusCode != tc.state || !strings.Contains(b, tc.want) {
			t.Errorf("POST %s %v = %d %s", tc.path, tc.form, res.StatusCode, b)
		}
	}

	// Rights: admin only.
	h.addUser("op", domain.RoleOperator)

	if res := h.signedIn("op").form(alice, url.Values{"role": {"admin"}}, false); res.StatusCode != http.StatusForbidden {
		t.Errorf("operator = %d", res.StatusCode)
	}

	anon := h.client()
	anon.session()

	if res := anon.form(alice, url.Values{"role": {"admin"}}, false); res.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Errorf("anonymous = %d", res.StatusCode)
	}
}
