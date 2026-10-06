package http_test

import (
	"net/http"
	"net/url"
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

	// The API twin of "Send a test e-mail".
	sent := h.mail.count()

	res = root.api(http.MethodPost, "/api/v1/mail/test", "")
	if v := decode(t, res); res.StatusCode != http.StatusOK || v["to"] != "root@example.org" {
		t.Errorf("API test mail = %d %v", res.StatusCode, v)
	}

	if m, _ := h.mail.last("root@example.org"); h.mail.count() != sent+1 || !strings.Contains(m.Subject, "Test") {
		t.Errorf("API test mail = %+v", m)
	}

	if res := h.signedIn("bob").api(http.MethodPost, "/api/v1/mail/test", ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("API test mail as listener = %d", res.StatusCode)
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

func TestInvitationAPI(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)

	root := h.signedIn("root")

	res := root.api(http.MethodPost, "/api/v1/invitations", `{"role":"listener","expires_in_hours":48}`)
	v := decode(t, res)

	link, _ := v["link"].(string)
	if res.StatusCode != http.StatusCreated || v["mailed"] != false || !strings.HasPrefix(link, hubURL+"/invite/") {
		t.Fatalf("create = %d %v", res.StatusCode, v)
	}

	token := strings.TrimPrefix(link, hubURL+"/invite/")
	anon := h.client()
	anon.session()

	if v := decode(t, anon.api(http.MethodGet, "/api/v1/auth/invitations/"+token, "")); v["role"] != "listener" {
		t.Errorf("check = %v", v)
	}

	if res := anon.api(http.MethodPost, "/api/v1/auth/invitations/"+token+"/accept", `{"username":"api_user","password":"`+newPassword+`","role":"admin"}`); res.StatusCode != http.StatusBadRequest {
		t.Errorf("role in the acceptance request = %d", res.StatusCode)
	}

	res = anon.api(http.MethodPost, "/api/v1/auth/invitations/"+token+"/accept", `{"username":"api_user","password":"`+newPassword+`"}`)
	if v := decode(t, res); res.StatusCode != http.StatusCreated || v["authenticated"] != true || setCookie(res, "__Host-rx_session") == nil {
		t.Fatalf("accept = %d %v", res.StatusCode, v)
	}

	res = root.api(http.MethodGet, "/api/v1/invitations", "")
	list, _ := decode(t, res)["invitations"].([]any)

	if first, _ := list[0].(map[string]any); len(list) != 1 || first["state"] != "redeemed" {
		t.Errorf("list = %v", list)
	}

	id := list[0].(map[string]any)["id"].(string)
	if res := root.api(http.MethodDelete, "/api/v1/invitations/"+id, ""); res.StatusCode != http.StatusConflict {
		t.Errorf("revoke a used invitation = %d", res.StatusCode)
	}

	if res := anon.api(http.MethodGet, "/api/v1/auth/invitations/"+token, ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("used = %d", res.StatusCode)
	}

	if strings.Contains(h.logs.String(), token) {
		t.Error("an invitation token reached the logs")
	}
}
