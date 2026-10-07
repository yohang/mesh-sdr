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

	if b := body(t, c.do(http.MethodGet, "/account", "", "", nil)); !strings.Contains(b, "alice@example.org") || strings.Contains(b, "new@example.org") {
		t.Errorf("address changed before confirmation: %s", b)
	}

	anon.session()

	res = anon.form("/account/email/verify", url.Values{"token": {token}}, false)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "is confirmed") {
		t.Fatalf("confirm = %d %s", res.StatusCode, b)
	}

	if b := body(t, c.do(http.MethodGet, "/account", "", "", nil)); !strings.Contains(b, "new@example.org") {
		t.Errorf("account after confirmation = %s", b)
	}

	if m, _ := h.mail.last("alice@example.org"); !strings.Contains(m.Body, "new@example.org") {
		t.Errorf("no notice to the previous address: %+v", m)
	}

	if res := anon.form("/account/email/verify", url.Values{"token": {token}}, false); res.StatusCode != http.StatusNotFound {
		t.Errorf("second use = %d", res.StatusCode)
	}

	// The address of another account cannot be confirmed, and the answer
	// is the one of any dead link (SR-06).
	res = c.form("/account/email", url.Values{"email": {"BOB@example.org"}, "current_password": {password}}, true)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "We sent a confirmation link") {
		t.Fatalf("change to a taken address = %d %s", res.StatusCode, b)
	}

	_, link = h.mail.last("BOB@example.org")
	if res := anon.form("/account/email/verify", url.Values{"token": {strings.TrimPrefix(link, "/account/email/verify/")}}, false); res.StatusCode != http.StatusNotFound {
		t.Errorf("taken address = %d", res.StatusCode)
	}
}
