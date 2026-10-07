package http_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func TestUsersPages(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)
	h.addUserWithEmail("alice", "alice@example.org", domain.RoleListener)
	h.addUser("bob", domain.RoleOperator)

	root := h.signedIn("root")
	alice := h.signedIn("alice")

	if res := alice.do(http.MethodGet, "/admin/users", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener = %d", res.StatusCode)
	}

	res := root.do(http.MethodGet, "/admin/users?q=example", "", "", nil)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "alice@example.org") || strings.Contains(b, ">bob<") {
		t.Fatalf("search = %d %s", res.StatusCode, b)
	}

	res = root.do(http.MethodGet, "/admin/users?role=operator", "", "", nil)
	if b := body(t, res); !strings.Contains(b, ">bob<") || strings.Contains(b, ">alice<") {
		t.Errorf("role filter = %s", b)
	}

	page := "/admin/users/" + h.userID("alice")

	res = root.do(http.MethodGet, page, "", "", nil)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "user-roles-title") || !strings.Contains(b, "Sign out everywhere") {
		t.Errorf("detail = %d %s", res.StatusCode, b)
	}

	if res := root.do(http.MethodGet, "/admin/users/not-an-id", "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown user = %d", res.StatusCode)
	}

	// Roles: operator on two devices.
	res = root.form(page+"/roles", url.Values{"role": {"listener"}, "devices": {"rtl-1, rtl-2"}}, false)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "operator@rtl-1,operator@rtl-2") {
		t.Errorf("roles = %d %s", res.StatusCode, b)
	}

	if s := alice.session(); s["authenticated"] != false {
		t.Error("session survived a role change")
	}

	if res := root.form(page+"/roles", url.Values{"role": {"listener"}, "devices": {"Bad Device"}}, false); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad device = %d", res.StatusCode)
	}

	// Reset link: e-mailed (mail is configured in the test hub).
	if b := body(t, root.form(page+"/password-reset", nil, false)); !strings.Contains(b, "e-mailed") {
		t.Errorf("reset = %s", b)
	}

	// Generated password, shown once.
	res = root.form(page+"/password", nil, false)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "Generated password (shown once)") {
		t.Errorf("generated = %d %s", res.StatusCode, b)
	}

	// Disable, enable; the last admin stays.
	if b := body(t, root.form(page+"/disable", nil, false)); !strings.Contains(b, "is disabled") {
		t.Errorf("disable = %s", b)
	}

	if b := body(t, root.form(page+"/enable", nil, false)); !strings.Contains(b, "is enabled") {
		t.Errorf("enable = %s", b)
	}

	res = root.form("/admin/users/"+h.userID("root")+"/disable", nil, false)
	if b := body(t, res); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(b, "last enabled admin") {
		t.Errorf("disable the last admin = %d %s", res.StatusCode, b)
	}
}
