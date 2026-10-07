package http_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
)

const newPassword = "a fresh and long passphrase"

func (c *client) changePassword(current, next, confirm, nextURL string, header map[string]string) *http.Response {
	c.h.t.Helper()

	form := url.Values{"current_password": {current}, "new_password": {next}, "confirm_password": {confirm}}
	if nextURL != "" {
		form.Set("next", nextURL)
	}

	h := map[string]string{identityhttp.CSRFHeader: c.token}
	for k, v := range header {
		h[k] = v
	}

	return c.do(http.MethodPost, identityhttp.PasswordChangePath, "application/x-www-form-urlencoded", form.Encode(), h)
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func TestForcedPasswordChange(t *testing.T) {
	h := newHub(t)
	pw := h.addFlagged("alice", domain.RoleListener)

	c := h.client()
	c.session()
	c.login("alice", pw, false)
	c.session()

	res := c.do(http.MethodGet, identityhttp.PasswordChangePath+"?forced=1&next=%2Freceiver", "", "", nil)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "password-forced") ||
		!strings.Contains(b, `name="next" value="/receiver"`) || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("page = %d %s", res.StatusCode, b)
	}

	for _, tc := range []struct {
		name, current, next, confirm string
		status                       int
		want                         string
	}{
		{"mismatch", pw, newPassword, newPassword + "x", http.StatusUnprocessableEntity, "do not match"},
		{"wrong current", "not the password", newPassword, newPassword, http.StatusUnprocessableEntity, "current password is incorrect"},
		{"too short", pw, "short", "short", http.StatusUnprocessableEntity, "at least 10 characters"},
		{"common", pw, "qwertyuiop", "qwertyuiop", http.StatusUnprocessableEntity, "too common"},
		{"same as current", pw, pw, pw, http.StatusUnprocessableEntity, "differ from the current"},
	} {
		res := c.changePassword(tc.current, tc.next, tc.confirm, "/receiver", map[string]string{"HX-Request": "true"})
		if b := body(t, res); res.StatusCode != tc.status || !strings.Contains(b, tc.want) || !strings.Contains(b, `role="alert"`) {
			t.Errorf("%s: %d %s", tc.name, res.StatusCode, b)
		}
	}

	old := *c.cookies["__Host-rx_session"]

	res = c.changePassword(pw, newPassword, newPassword, "/receiver", nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/receiver" {
		t.Fatalf("change = %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	sc := setCookie(res, "__Host-rx_session")
	if sc == nil || sc.Value == old.Value || sc.MaxAge != 0 {
		t.Fatalf("session cookie = %+v", sc)
	}

	// The replaced session is dead, the new one is free to go anywhere.
	stale := h.client()
	stale.cookies[old.Name] = &old

	if v := stale.session(); v["authenticated"] != false {
		t.Error("replaced session still valid")
	}

	s := c.session()
	if user, _ := s["user"].(map[string]any); s["authenticated"] != true || user["must_change_password"] != false {
		t.Errorf("session = %v", s)
	}

	if res := c.do(http.MethodGet, "/receiver", "", "", nil); res.StatusCode != http.StatusOK {
		t.Errorf("GET /receiver = %d", res.StatusCode)
	}

	// The new password signs in, the generated one no longer does.
	other := h.client()
	other.session()

	if res := other.login("alice", pw, false); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("old password login = %d", res.StatusCode)
	}

	if res := other.login("alice", newPassword, false); res.StatusCode != http.StatusSeeOther {
		t.Errorf("new password login = %d", res.StatusCode)
	}
}

func TestPasswordPageNeedsSession(t *testing.T) {
	h := newHub(t)
	c := h.client()

	res := c.do(http.MethodGet, identityhttp.PasswordChangePath, "", "", nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login?next=%2Faccount%2Fpassword" {
		t.Errorf("anonymous GET = %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	c.session()

	if res := c.changePassword(password, newPassword, newPassword, "", nil); res.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Errorf("anonymous POST = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestPasswordChangeKeepsRememberMe(t *testing.T) {
	h := newHub(t)
	h.addUser("bob", domain.RoleListener)

	c := h.client()
	c.session()
	c.login("bob", password, true)
	c.session()

	res := c.changePassword(password, newPassword, newPassword, "", nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != identityhttp.PasswordChangePath+"?changed=1" {
		t.Fatalf("change = %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	if sc := setCookie(res, "__Host-rx_session"); sc == nil || sc.MaxAge < 29*24*3600 || sc.MaxAge > 30*24*3600 {
		t.Errorf("cookie = %+v", sc)
	}
}

func TestVoluntaryPasswordChange(t *testing.T) {
	h := newHub(t)
	h.addUser("bob", domain.RoleOperator)

	c := h.client()
	c.session()
	c.login("bob", password, false)
	c.session()

	other := h.client()
	other.session()
	other.login("bob", password, false)

	for _, tc := range []struct{ current, next string }{
		{"wrong password!", newPassword},
		{password, "short"},
		{password, "qwertyuiop"},
		{password, password},
	} {
		res := c.changePassword(tc.current, tc.next, tc.next, "", nil)
		if b := body(t, res); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(b, `role="alert"`) {
			t.Errorf("%q → %d %s", tc.next, res.StatusCode, b)
		}
	}

	// The CSRF header is required.
	form := url.Values{"current_password": {password}, "new_password": {newPassword}, "confirm_password": {newPassword}}
	if res := c.do(http.MethodPost, identityhttp.PasswordChangePath, "application/x-www-form-urlencoded", form.Encode(), nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("without CSRF = %d", res.StatusCode)
	}

	before := c.token

	res := c.changePassword(password, newPassword, newPassword, "", nil)
	if res.StatusCode != http.StatusSeeOther || setCookie(res, "__Host-rx_session") == nil {
		t.Fatalf("change = %d", res.StatusCode)
	}

	// The caller keeps a session (the new one), its other sessions are gone.
	if s := c.session(); s["authenticated"] != true || s["csrf_token"] == before {
		t.Errorf("caller session = %v", s)
	}

	if s := other.session(); s["authenticated"] != false {
		t.Error("other session still valid")
	}

	page := c.do(http.MethodGet, identityhttp.PasswordChangePath+"?changed=1", "", "", nil)
	if b := body(t, page); !strings.Contains(b, "password-changed") || strings.Contains(b, "password-forced") {
		t.Errorf("changed page = %s", b)
	}
}

// TestUnknownBodyFieldsAreRefused: JSON bodies of the API are closed
// (SR-20).
func TestUnknownBodyFieldsAreRefused(t *testing.T) {
	h := newHub(t)
	c := h.client()
	c.session()

	res := c.do(http.MethodPost, "/api/v1/auth/token", "application/json",
		`{"node_id":"roof","cid":"x","role":"admin"}`, map[string]string{identityhttp.CSRFHeader: c.token})
	if v := decode(t, res); res.StatusCode != http.StatusBadRequest || v["code"] != "unknown_field" {
		t.Errorf("token request with an unknown field = %d %v", res.StatusCode, v)
	}
}
