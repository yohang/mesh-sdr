package http_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
)

func (h *hub) beginSetup() string {
	h.t.Helper()

	u, err := h.setup.Begin(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}

	return strings.TrimPrefix(u, hubURL)
}

func (c *client) postSetup(token, username, pw, confirm string) *http.Response {
	c.h.t.Helper()

	form := url.Values{"token": {token}, "username": {username}, "email": {"root@example.org"}, "password": {pw}, "confirm_password": {confirm}}

	return c.do(http.MethodPost, "/setup", "application/x-www-form-urlencoded", form.Encode(), map[string]string{identityhttp.CSRFHeader: c.token})
}

func TestSetupCreatesTheFirstAdmin(t *testing.T) {
	h := newHub(t)
	h.addUser("listener", domain.RoleListener)

	path := h.beginSetup()
	if !strings.HasPrefix(path, "/setup/") {
		t.Fatalf("setup URL path = %q", path)
	}

	token := strings.TrimPrefix(path, "/setup/")

	c := h.client()
	c.session()

	res := c.do(http.MethodGet, path, "", "", nil)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, `data-replace-url="/setup"`) ||
		!strings.Contains(b, `name="token" value="`+token+`"`) {
		t.Fatalf("setup page = %d %s", res.StatusCode, b)
	}

	if res.Header.Get("Referrer-Policy") != "no-referrer" || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", res.Header)
	}

	for _, tc := range []struct{ name, username, pw, confirm, want string }{
		{"mismatch", "root", newPassword, "other", "do not match"},
		{"bad username", "r", newPassword, newPassword, "username"},
		{"taken username", "LISTENER", newPassword, newPassword, "already used"},
		{"common password", "root", "password123", "password123", "too common"},
	} {
		res := c.postSetup(token, tc.username, tc.pw, tc.confirm)
		if b := body(t, res); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(b, tc.want) {
			t.Errorf("%s: %d %s", tc.name, res.StatusCode, b)
		}
	}

	res = c.postSetup(token, "root", newPassword, newPassword)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" || setCookie(res, "__Host-rx_session") == nil {
		t.Fatalf("setup = %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	s := c.session()
	if roles, _ := s["roles"].([]any); s["authenticated"] != true || len(roles) != 2 || roles[1] != "admin" {
		t.Errorf("session after setup = %v", s)
	}

	if res := c.do(http.MethodGet, "/admin", "", "", nil); res.StatusCode != http.StatusOK {
		t.Errorf("admin page = %d", res.StatusCode)
	}

	// The link is used up, and an admin now exists: no new link.
	if res := h.client().do(http.MethodGet, path, "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("used link = %d", res.StatusCode)
	}

	if again := h.beginSetup(); again != "" {
		t.Errorf("new link with an admin: %q", again)
	}
}

func TestSetupRefusals(t *testing.T) {
	h := newHub(t, func(c *config.Hub) { c.Admin.AllowedNetworks = []string{"10.0.0.0/8"} })
	path := h.beginSetup()

	c := h.client()

	if res := c.do(http.MethodGet, path, "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("outside admin networks = %d", res.StatusCode)
	}

	c.session()

	if res := c.postSetup(strings.TrimPrefix(path, "/setup/"), "root", newPassword, newPassword); res.StatusCode != http.StatusForbidden {
		t.Errorf("POST outside admin networks = %d", res.StatusCode)
	}

	c.remote = "10.1.2.3:4444"

	for _, p := range []string{"/setup/not-the-token", "/setup"} {
		res := c.do(http.MethodGet, p, "", "", nil)
		if b := body(t, res); res.StatusCode != http.StatusNotFound || !strings.Contains(b, "Setup link not valid") {
			t.Errorf("GET %s = %d %s", p, res.StatusCode, b)
		}
	}

	c.session()

	if res := c.postSetup("not-the-token", "root", newPassword, newPassword); res.StatusCode != http.StatusNotFound {
		t.Errorf("wrong token = %d", res.StatusCode)
	}

	// Setup requests are rate-limited per client address.
	var last *http.Response
	for range 12 {
		last = c.do(http.MethodGet, "/setup/not-the-token", "", "", nil)
	}

	if last.StatusCode != http.StatusTooManyRequests || last.Header.Get("Retry-After") == "" {
		t.Errorf("flood = %d", last.StatusCode)
	}
}
