package http_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
)

// addFlagged creates a user with a generated password (must_change_password)
// and returns that password.
func (h *hub) addFlagged(name string, role domain.Role) string {
	h.t.Helper()

	res, err := h.admin.Add(context.Background(), app.AddUserInput{Username: name, Role: role})
	if err != nil {
		h.t.Fatal(err)
	}

	if !res.User.MustChangePassword() || res.GeneratedPassword == "" {
		h.t.Fatal("user not flagged")
	}

	return res.GeneratedPassword
}

func TestPasswordGateConfinesFlaggedUsers(t *testing.T) {
	h := newHub(t)
	pw := h.addFlagged("alice", domain.RoleAdmin)

	c := h.client()
	c.session()

	if res := c.login("alice", pw, false); res.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", res.StatusCode)
	}

	s := c.session()
	if user, _ := s["user"].(map[string]any); user["must_change_password"] != true {
		t.Fatalf("session = %v", s)
	}

	// Pages, anonymous ones included, redirect to the change page with a
	// safe next.
	for _, tc := range []struct{ path, want string }{
		{"/receiver", identityhttp.PasswordChangePath + "?forced=1&next=%2Freceiver"},
		{"/admin", identityhttp.PasswordChangePath + "?forced=1&next=%2Fadmin"},
		{"/login", identityhttp.PasswordChangePath + "?forced=1&next=%2Flogin"},
		{"/no-such-page", identityhttp.PasswordChangePath + "?forced=1"},
	} {
		res := c.do(http.MethodGet, tc.path, "", "", nil)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != tc.want {
			t.Errorf("GET %s = %d %q, want 303 %q", tc.path, res.StatusCode, res.Header.Get("Location"), tc.want)
		}
	}

	// htmx requests get HX-Redirect.
	res := c.do(http.MethodGet, "/receiver", "", "", map[string]string{"HX-Request": "true"})
	if res.StatusCode != http.StatusNoContent || !strings.HasPrefix(res.Header.Get("HX-Redirect"), identityhttp.PasswordChangePath) {
		t.Errorf("htmx GET = %d %q", res.StatusCode, res.Header.Get("HX-Redirect"))
	}

	// Unsafe page requests are redirected without a next.
	res = c.do(http.MethodPost, "/login", "application/x-www-form-urlencoded", "login=x&password=y", map[string]string{identityhttp.CSRFHeader: c.token})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != identityhttp.PasswordChangePath+"?forced=1" {
		t.Errorf("POST /login = %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	// API calls other than the allowed ones are refused.
	res = c.do(http.MethodGet, "/api/v1/no-such-endpoint", "", "", nil)
	if v := decode(t, res); res.StatusCode != http.StatusForbidden || v["code"] != "password_change_required" {
		t.Errorf("API = %d %v", res.StatusCode, v)
	}

	// Allowed: session API, health, static resources, logout.
	for _, path := range []string{"/api/v1/auth/session", "/api/v1/healthz/live", "/api/v1/openapi.json"} {
		if res := c.do(http.MethodGet, path, "", "", nil); res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, res.StatusCode)
		}
	}

	if res := c.do(http.MethodGet, "/static/no-such-file.js", "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("static = %d, want the file server's 404", res.StatusCode)
	}

	res = c.do(http.MethodPost, "/api/v1/auth/logout", "", "", map[string]string{identityhttp.CSRFHeader: c.token})
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("logout = %d", res.StatusCode)
	}
}

func TestPasswordGateIgnoresOtherUsers(t *testing.T) {
	h := newHub(t)
	h.addUser("bob", domain.RoleListener)

	c := h.client()
	c.session()
	c.login("bob", password, false)

	if res := c.do(http.MethodGet, "/receiver", "", "", nil); res.StatusCode != http.StatusOK {
		t.Errorf("GET /receiver = %d", res.StatusCode)
	}

	anon := h.client()
	if res := anon.do(http.MethodGet, "/receiver", "", "", nil); res.StatusCode != http.StatusOK {
		t.Errorf("anonymous GET /receiver = %d", res.StatusCode)
	}
}

// The forced-change gate allow-lists by the decoded path only when it is
// also the path chi routes on.
func TestPasswordGateRefusesEncodedPaths(t *testing.T) {
	h := newHub(t)
	pw := h.addFlagged("alice", domain.RoleListener)

	c := h.client()
	c.session()
	c.login("alice", pw, false)

	for _, p := range []string{"/account%2Fpassword", "/static%2F..%2Freceiver", "/api/v1/auth%2Fsession"} {
		res := c.do(http.MethodGet, p, "", "", nil)
		if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s = %d, want refused", p, res.StatusCode)
		}

		if loc := res.Header.Get("Location"); res.StatusCode == http.StatusSeeOther && !strings.HasPrefix(loc, identityhttp.PasswordChangePath) {
			t.Errorf("GET %s → %q", p, loc)
		}
	}
}
