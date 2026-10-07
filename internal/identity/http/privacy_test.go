package http_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func TestExportAndDeletion(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)
	h.addUserWithEmail("alice", "alice@example.org", domain.RoleListener)
	h.addUser("bob", domain.RoleListener)

	alice := h.signedIn("alice")

	if res := alice.do(http.MethodGet, "/account/export", "", "", nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET export = %d", res.StatusCode)
	}

	if res := alice.do(http.MethodPost, "/account/export", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("export without CSRF = %d", res.StatusCode)
	}

	res := alice.form("/account/export", nil, false)

	var exp struct {
		Account struct {
			Username string `json:"username"`
			Email    string `json:"email"`
		} `json:"account"`
		Sessions []any `json:"sessions"`
	}

	if err := json.NewDecoder(res.Body).Decode(&exp); err != nil || res.StatusCode != http.StatusOK ||
		!strings.Contains(res.Header.Get("Content-Disposition"), "attachment") || exp.Account.Email != "alice@example.org" || len(exp.Sessions) != 1 {
		t.Fatalf("export = %d %+v %v", res.StatusCode, exp, err)
	}

	res = alice.form("/account/delete", url.Values{"current_password": {password}}, true)
	if b := body(t, res); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(b, "Tick the box") {
		t.Errorf("without confirmation = %d %s", res.StatusCode, b)
	}

	res = alice.form("/account/delete", url.Values{"current_password": {password}, "confirm": {"1"}}, false)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("delete = %d %s", res.StatusCode, body(t, res))
	}

	if s := alice.session(); s["authenticated"] != false {
		t.Error("session survived the deletion")
	}

	// Admin › Users: export and delete, admin only; the last admin stays.
	root := h.signedIn("root")
	bob := "/admin/users/" + h.userID("bob")

	if res := root.form(bob+"/export", nil, false); res.StatusCode != http.StatusOK ||
		!strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("admin export = %d", res.StatusCode)
	}

	if res := h.signedIn("bob").form(bob+"/delete", url.Values{"confirm": {"1"}}, false); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener deletes = %d", res.StatusCode)
	}

	res = root.form("/admin/users/"+h.userID("root")+"/delete", url.Values{"confirm": {"1"}}, false)
	if b := body(t, res); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(b, "last enabled admin") {
		t.Errorf("delete the last admin = %d %s", res.StatusCode, b)
	}

	if res := root.form(bob+"/delete", url.Values{"confirm": {"1"}}, false); res.StatusCode != http.StatusSeeOther {
		t.Errorf("admin deletes = %d %s", res.StatusCode, body(t, res))
	}
}
