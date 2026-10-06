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

	res := alice.do(http.MethodGet, "/account/export", "", "", nil)

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

	// API: export and delete as admin, delete own.
	root := h.signedIn("root")
	bobID := h.userID("bob")

	if res := root.api(http.MethodGet, "/api/v1/users/"+bobID+"/export", ""); res.StatusCode != http.StatusOK {
		t.Errorf("admin export = %d", res.StatusCode)
	}

	bob := h.signedIn("bob")
	if res := bob.api(http.MethodDelete, "/api/v1/users/"+bobID, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener deletes = %d", res.StatusCode)
	}

	if res := bob.api(http.MethodDelete, "/api/v1/me", `{"current_password":"`+password+`"}`); res.StatusCode != http.StatusNoContent {
		t.Errorf("delete me = %d", res.StatusCode)
	}

	if res := root.api(http.MethodDelete, "/api/v1/users/"+h.userID("root"), ""); res.StatusCode != http.StatusConflict {
		t.Errorf("delete the last admin = %d", res.StatusCode)
	}
}
