package http_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
)

func (h *hub) addUserWithEmail(name, email string, role domain.Role) {
	h.t.Helper()

	if _, err := h.admin.Add(context.Background(), app.AddUserInput{Username: name, Email: email, Role: role, Password: password}); err != nil {
		h.t.Fatal(err)
	}
}

func (c *client) form(path string, values url.Values, htmx bool) *http.Response {
	c.h.t.Helper()

	header := map[string]string{identityhttp.CSRFHeader: c.token}
	if htmx {
		header["HX-Request"] = "true"
	}

	return c.do(http.MethodPost, path, "application/x-www-form-urlencoded", values.Encode(), header)
}

func TestAccountPage(t *testing.T) {
	h := newHub(t)
	h.addUserWithEmail("alice", "alice@example.org", domain.RoleListener)

	if res := h.client().do(http.MethodGet, "/account", "", "", nil); res.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Errorf("anonymous = %d", res.StatusCode)
	}

	c := h.signedIn("alice")
	other := h.signedIn("alice")

	res := c.do(http.MethodGet, "/account", "", "", nil)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "account-profile") ||
		!strings.Contains(b, "alice@example.org") || !strings.Contains(b, "(this session)") || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("page = %d %s", res.StatusCode, b)
	}

	// Display name: the section alone comes back to htmx.
	res = c.form("/account/profile", url.Values{"display_name": {"Alice <b>A</b>"}}, true)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "was saved") ||
		strings.Contains(b, "account-email") || strings.Contains(b, "<b>A</b>") {
		t.Errorf("profile = %d %s", res.StatusCode, b)
	}

	res = c.form("/account/profile", url.Values{"display_name": {strings.Repeat("x", 65)}}, true)
	if b := body(t, res); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(b, `role="alert"`) {
		t.Errorf("long name = %d %s", res.StatusCode, b)
	}

	// Sessions: sign out the other one.
	res = c.form("/account/sessions/revoke-others", nil, true)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "1 other session(s) signed out") {
		t.Errorf("revoke others = %d %s", res.StatusCode, b)
	}

	if s := other.session(); s["authenticated"] != false {
		t.Error("other session survived")
	}
}

func TestEmailChangeWithConfirmation(t *testing.T) {
	h := newHub(t)
	h.addUserWithEmail("alice", "alice@example.org", domain.RoleListener)
	h.addUserWithEmail("bob", "bob@example.org", domain.RoleListener)

	c := h.signedIn("alice")

	res := c.form("/account/email", url.Values{"email": {"new@example.org"}, "current_password": {"wrong password"}}, true)
	if b := body(t, res); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(b, "current password is incorrect") {
		t.Errorf("wrong password = %d %s", res.StatusCode, b)
	}

	res = c.form("/account/email", url.Values{"email": {"new@example.org"}, "current_password": {password}}, true)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "We sent a confirmation link") {
		t.Fatalf("change = %d %s", res.StatusCode, b)
	}

	msg, link := h.mail.last("new@example.org")
	if !strings.HasPrefix(link, "/account/email/verify/") {
		t.Fatalf("confirmation mail = %+v", msg)
	}

	token := strings.TrimPrefix(link, "/account/email/verify/")

	// Opening the link changes nothing until confirmed (GET is safe).
	anon := h.client()
	res = anon.do(http.MethodGet, link, "", "", nil)

	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, `data-replace-url="/account/email/verify"`) ||
		res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("verify page = %d %s", res.StatusCode, b)
	}

	if v := decode(t, c.api(http.MethodGet, "/api/v1/me", "")); v["email"] != "alice@example.org" {
		t.Errorf("address changed before confirmation: %v", v)
	}

	anon.session()

	res = anon.form("/account/email/verify", url.Values{"token": {token}}, false)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "is confirmed") {
		t.Fatalf("confirm = %d %s", res.StatusCode, b)
	}

	if v := decode(t, c.api(http.MethodGet, "/api/v1/me", "")); v["email"] != "new@example.org" || v["email_verified"] != true {
		t.Errorf("me = %v", v)
	}

	if m, _ := h.mail.last("alice@example.org"); !strings.Contains(m.Body, "new@example.org") {
		t.Errorf("no notice to the previous address: %+v", m)
	}

	if res := anon.form("/account/email/verify", url.Values{"token": {token}}, false); res.StatusCode != http.StatusNotFound {
		t.Errorf("second use = %d", res.StatusCode)
	}

	// The address of another account cannot be confirmed.
	res = c.api(http.MethodPost, "/api/v1/me/email", `{"email":"BOB@example.org","current_password":"`+password+`"}`)
	if v := decode(t, res); res.StatusCode != http.StatusOK || v["pending"] != true {
		t.Fatalf("API change = %d %v", res.StatusCode, v)
	}

	_, link = h.mail.last("BOB@example.org")
	if res := anon.form("/account/email/verify", url.Values{"token": {strings.TrimPrefix(link, "/account/email/verify/")}}, false); res.StatusCode != http.StatusConflict {
		t.Errorf("taken address = %d", res.StatusCode)
	}
}

func TestMeAPI(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleOperator)

	c := h.signedIn("alice")

	v := decode(t, c.api(http.MethodGet, "/api/v1/me", ""))
	if v["username"] != "alice" || v["email_verified"] != false || len(v["roles"].([]any)) != 1 || v["roles"].([]any)[0] != "operator" {
		t.Errorf("me = %v", v)
	}

	res := c.api(http.MethodPatch, "/api/v1/me", `{"display_name":"Alice"}`)
	if v := decode(t, res); res.StatusCode != http.StatusOK || v["display_name"] != "Alice" {
		t.Errorf("patch = %d %v", res.StatusCode, v)
	}

	if res := c.api(http.MethodPatch, "/api/v1/me", `{"display_name":"Alice","username":"root"}`); res.StatusCode != http.StatusBadRequest {
		t.Errorf("patch with an unknown field = %d", res.StatusCode)
	}

	if res := h.client().api(http.MethodGet, "/api/v1/me", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", res.StatusCode)
	}
}
