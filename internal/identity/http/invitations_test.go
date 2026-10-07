package http_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func TestInvitationPages(t *testing.T) {
	h := newHub(t)
	h.addUserWithEmail("root", "root@example.org", domain.RoleAdmin)
	h.addUser("bob", domain.RoleListener)

	root := h.signedIn("root")

	if res := h.signedIn("bob").do(http.MethodGet, "/admin/invitations", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener = %d", res.StatusCode)
	}

	res := root.form("/admin/invitations", url.Values{"role": {"operator"}, "email": {"inv@example.org"}, "days": {"3"}}, false)

	b := body(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(b, "was e-mailed to inv@example.org") {
		t.Fatalf("create = %d %s", res.StatusCode, b)
	}

	_, link := h.mail.last("inv@example.org")
	if !strings.HasPrefix(link, "/invite/") || !strings.Contains(b, link) {
		t.Fatalf("link %q not shown", link)
	}

	if res := root.form("/admin/invitations/test-mail", nil, false); res.StatusCode != http.StatusOK {
		t.Errorf("test mail = %d", res.StatusCode)
	}

	if m, _ := h.mail.last("root@example.org"); !strings.Contains(m.Subject, "Test") {
		t.Errorf("test mail = %+v", m)
	}

	if res := h.signedIn("bob").form("/admin/invitations/test-mail", nil, false); res.StatusCode != http.StatusForbidden {
		t.Errorf("test mail as listener = %d", res.StatusCode)
	}

	// The invitee: the page, then the account.
	invitee := h.client()

	res = invitee.do(http.MethodGet, link, "", "", nil)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "<strong>operator</strong>") ||
		!strings.Contains(b, "inv@example.org") || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("invite page = %d %s", res.StatusCode, b)
	}

	invitee.session()

	token := strings.TrimPrefix(link, "/invite/")
	res = invitee.form("/invite", url.Values{"token": {token}, "username": {"newop"}, "password": {newPassword}, "confirm_password": {newPassword}}, false)

	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("accept = %d %s", res.StatusCode, body(t, res))
	}

	if s := invitee.session(); s["authenticated"] != true || len(s["roles"].([]any)) != 2 {
		t.Errorf("session = %v", s)
	}

	// Used: one message for every reason.
	other := h.client()
	if res := other.do(http.MethodGet, link, "", "", nil); res.StatusCode != http.StatusNotFound ||
		!strings.Contains(body(t, res), "no longer valid") {
		t.Errorf("used link = %d", res.StatusCode)
	}

	if res := other.do(http.MethodGet, "/invite/garbage", "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("garbage = %d", res.StatusCode)
	}

	if h.logs.String() != "" && strings.Contains(h.logs.String(), token) {
		t.Error("an invitation token reached the logs")
	}
}

// TestInvitationLinkAndRevocation: an invitation shown as a link; the
// role comes from the invitation, never from the acceptance form; a used
// invitation cannot be revoked.
func TestInvitationLinkAndRevocation(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)

	root := h.signedIn("root")

	res := root.form("/admin/invitations", url.Values{"role": {"listener"}, "days": {"2"}}, false)
	b := body(t, res)

	link := regexp.MustCompile(`/invite/[A-Za-z0-9_-]+`).FindString(b)
	if res.StatusCode != http.StatusOK || link == "" {
		t.Fatalf("create = %d %s", res.StatusCode, b)
	}

	revoke := regexp.MustCompile(`/admin/invitations/[0-9a-f-]+/revoke`).FindString(body(t, root.do(http.MethodGet, "/admin/invitations", "", "", nil)))
	if revoke == "" {
		t.Fatal("no revoke form")
	}

	token := strings.TrimPrefix(link, "/invite/")
	anon := h.client()
	anon.session()

	res = anon.form("/invite", url.Values{
		"token": {token}, "role": {"admin"}, "username": {"invitee"}, "password": {newPassword}, "confirm_password": {newPassword},
	}, false)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("accept = %d %s", res.StatusCode, body(t, res))
	}

	if s := anon.session(); s["authenticated"] != true || len(s["roles"].([]any)) != 1 || s["roles"].([]any)[0] != "listener" {
		t.Errorf("session = %v", s)
	}

	if res := root.form(revoke, nil, false); res.StatusCode == http.StatusOK {
		t.Errorf("revoke a used invitation = %d", res.StatusCode)
	}

	if res := h.client().do(http.MethodGet, link, "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("used = %d", res.StatusCode)
	}

	if strings.Contains(h.logs.String(), token) {
		t.Error("an invitation token reached the logs")
	}
}
