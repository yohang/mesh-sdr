package http_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// waitMail waits for a message to an address (reset mails are sent after
// the answer).
func (h *hub) waitMail(to string) string {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if _, link := h.mail.last(to); link != "" {
			return link
		}

		time.Sleep(10 * time.Millisecond)
	}

	h.t.Fatalf("no mail to %s", to)

	return ""
}

func TestPasswordResetPages(t *testing.T) {
	h := newHub(t)
	h.addUserWithEmail("alice", "alice@example.org", domain.RoleListener)

	signedIn := h.signedIn("alice")
	c := h.client()

	if b := body(t, c.do(http.MethodGet, "/login", "", "", nil)); !strings.Contains(b, `href="/password/forgot"`) {
		t.Error("no forgot-password link on the login page")
	}

	c.session()

	known := body(t, c.form("/password/forgot", url.Values{"login": {"alice"}}, true))
	unknown := body(t, c.form("/password/forgot", url.Values{"login": {"nobody"}}, true))

	if known != unknown || !strings.Contains(known, "If an account matches") {
		t.Errorf("answers differ (SR-06):\n%s\n%s", known, unknown)
	}

	link := h.waitMail("alice@example.org")
	if !strings.HasPrefix(link, "/password/reset/") {
		t.Fatalf("link = %q", link)
	}

	res := c.do(http.MethodGet, link, "", "", nil)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, `data-replace-url="/password/reset"`) ||
		res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("reset page = %d %s", res.StatusCode, b)
	}

	token := strings.TrimPrefix(link, "/password/reset/")

	res = c.form("/password/reset", url.Values{"token": {token}, "password": {newPassword}, "confirm_password": {newPassword}}, false)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login?reset=1" {
		t.Fatalf("reset = %d %s", res.StatusCode, body(t, res))
	}

	if b := body(t, c.do(http.MethodGet, "/login?reset=1", "", "", nil)); !strings.Contains(b, "Your password was reset") {
		t.Error("no success banner")
	}

	if s := signedIn.session(); s["authenticated"] != false {
		t.Error("session survived the reset")
	}

	if res := c.do(http.MethodGet, link, "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("used link = %d", res.StatusCode)
	}

	if strings.Contains(h.logs.String(), token) {
		t.Error("a reset token reached the logs")
	}
}

func TestPasswordResetAPI(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)
	h.addUser("bob", domain.RoleListener)
	h.addUser("carol", domain.RoleListener)

	anon := h.client()
	anon.session()

	if res := anon.api(http.MethodPost, "/api/v1/auth/password-reset", `{"login":"bob"}`); res.StatusCode != http.StatusAccepted {
		t.Errorf("request = %d", res.StatusCode)
	}

	root := h.signedIn("root")

	res := root.api(http.MethodPost, "/api/v1/users/"+h.userID("bob")+"/password-reset", "")
	v := decode(t, res)

	link, _ := v["link"].(string)
	if res.StatusCode != http.StatusOK || v["mailed"] != false || link == "" {
		t.Fatalf("admin reset = %d %v", res.StatusCode, v)
	}

	token := strings.TrimPrefix(link, hubURL+"/password/reset/")

	res = anon.api(http.MethodPost, "/api/v1/auth/password-reset/confirm", `{"token":"`+token+`","new_password":"`+newPassword+`"}`)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("confirm = %d %v", res.StatusCode, decode(t, res))
	}

	if res := anon.login("bob", newPassword, false); res.StatusCode != http.StatusOK {
		t.Errorf("login = %d", res.StatusCode)
	}

	if res := h.signedIn("carol").api(http.MethodPost, "/api/v1/users/"+h.userID("root")+"/password-reset", ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener issues a reset = %d", res.StatusCode)
	}
}
