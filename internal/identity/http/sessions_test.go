package http_test

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// revokeForms returns the session revocation forms of a page under prefix.
func revokeForms(t *testing.T, res *http.Response, prefix string) []string {
	t.Helper()

	re := regexp.MustCompile(regexp.QuoteMeta(prefix) + `/sessions/[A-Za-z0-9_-]+/revoke`)

	return re.FindAllString(body(t, res), -1)
}

// TestSessionForms: own sessions on the account page, a user's sessions on
// Admin › Users, and who may sign them out (SR-18).
func TestSessionForms(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)
	h.addUser("alice", domain.RoleListener)
	h.addUser("bob", domain.RoleListener)

	alice := h.signedIn("alice")
	alice2 := h.signedIn("alice")
	bob := h.signedIn("bob")

	page := body(t, alice.do(http.MethodGet, "/account", "", "", nil))

	current := regexp.MustCompile(`(?s)\(this session\).*?(/account/sessions/[A-Za-z0-9_-]+/revoke)`).FindStringSubmatch(page)
	// Each form names its action twice (action and hx-post).
	forms := slices.Compact(regexp.MustCompile(`/account/sessions/[A-Za-z0-9_-]+/revoke`).FindAllString(page, -1))

	if current == nil || len(forms) != 2 {
		t.Fatalf("own sessions = %v", forms)
	}

	other := forms[0]
	if other == current[1] {
		other = forms[1]
	}

	// Bob cannot revoke Alice's sessions.
	for _, f := range forms {
		if res := bob.form(f, nil, false); res.StatusCode != http.StatusNotFound {
			t.Errorf("bob revokes %s = %d", f, res.StatusCode)
		}
	}

	if s := alice2.session(); s["authenticated"] != true {
		t.Fatal("bob signed out alice")
	}

	if res := alice.form(other, nil, false); res.StatusCode != http.StatusOK {
		t.Errorf("revoke = %d", res.StatusCode)
	}

	if s := alice2.session(); s["authenticated"] != false {
		t.Error("revoked session still valid")
	}

	if s := alice.session(); s["authenticated"] != true {
		t.Error("the current session was signed out")
	}

	// Admin side.
	root := h.signedIn("root")
	user := "/admin/users/" + h.userID("alice")

	if list := revokeForms(t, root.do(http.MethodGet, user, "", "", nil), user); len(list) != 1 {
		t.Errorf("admin list = %v", list)
	}

	if res := bob.do(http.MethodGet, user, "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener reads another user's sessions = %d", res.StatusCode)
	}

	if res := bob.form(user+"/sessions/revoke", nil, false); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener signs another user out = %d", res.StatusCode)
	}

	// A session of another user cannot be revoked through this user's page.
	bobPage := "/admin/users/" + h.userID("bob")

	bobForms := revokeForms(t, root.do(http.MethodGet, bobPage, "", "", nil), bobPage)
	if len(bobForms) != 1 {
		t.Fatalf("bob's sessions = %v", bobForms)
	}

	ref := regexp.MustCompile(`/sessions/([A-Za-z0-9_-]+)/revoke`).FindStringSubmatch(bobForms[0])[1]
	if res := root.form(user+"/sessions/"+ref+"/revoke", nil, false); res.StatusCode == http.StatusOK {
		t.Errorf("revoke bob's session through alice's page = %d", res.StatusCode)
	}

	if s := bob.session(); s["authenticated"] != true {
		t.Error("bob signed out through alice's page")
	}

	if res := root.form(user+"/sessions/revoke", nil, false); res.StatusCode != http.StatusOK {
		t.Errorf("revoke all = %d", res.StatusCode)
	}

	if s := alice.session(); s["authenticated"] != false {
		t.Error("session survived sign-out everywhere")
	}
}

// TestSignOutEverywhere: the account page signs out every session of the
// user, the current one included, publishes the revocation and goes to the
// login page.
func TestSignOutEverywhere(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleListener)
	h.addUser("bob", domain.RoleListener)

	alice := h.signedIn("alice")
	alice2 := h.signedIn("alice")
	bob := h.signedIn("bob")

	if b := body(t, alice.do(http.MethodGet, "/account", "", "", nil)); !strings.Contains(b, `action="/account/sessions/revoke-all"`) {
		t.Fatalf("no Sign out everywhere form: %s", b)
	}

	if res := alice.do(http.MethodPost, "/account/sessions/revoke-all", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("without CSRF = %d", res.StatusCode)
	}

	anon := h.client()
	anon.session()

	if res := anon.form("/account/sessions/revoke-all", nil, false); res.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(res.Header.Get("Location"), "/login?next=") {
		t.Errorf("anonymous = %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	before := len(h.revoked.sessions())

	res := alice.form("/account/sessions/revoke-all", nil, false)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Fatalf("sign out everywhere = %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	if ck := setCookie(res, "__Host-rx_session"); ck == nil || ck.MaxAge >= 0 {
		t.Errorf("cookie not cleared: %+v", ck)
	}

	if n := len(h.revoked.sessions()) - before; n != 2 {
		t.Errorf("published %d revoked sessions, want 2", n)
	}

	for name, c := range map[string]*client{"current": alice, "other": alice2} {
		if s := c.session(); s["authenticated"] != false {
			t.Errorf("%s session survived", name)
		}
	}

	if s := bob.session(); s["authenticated"] != true {
		t.Error("another user was signed out")
	}

	// htmx (boosted form) follows HX-Redirect.
	again := h.signedIn("alice")
	if res := again.form("/account/sessions/revoke-all", nil, true); res.StatusCode != http.StatusNoContent || res.Header.Get("HX-Redirect") != "/login" {
		t.Errorf("htmx = %d %q", res.StatusCode, res.Header.Get("HX-Redirect"))
	}
}
