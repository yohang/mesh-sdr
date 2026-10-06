package http_test

import (
	"net/http"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func sessionIDs(t *testing.T, res *http.Response) (current string, others []string) {
	t.Helper()

	v := decode(t, res)

	list, _ := v["sessions"].([]any)
	for _, raw := range list {
		s, _ := raw.(map[string]any)
		id, _ := s["id"].(string)

		if s["current"] == true {
			current = id
		} else {
			others = append(others, id)
		}
	}

	return current, others
}

func TestSessionsAPI(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)
	h.addUser("alice", domain.RoleListener)
	h.addUser("bob", domain.RoleListener)

	alice := h.signedIn("alice")
	alice2 := h.signedIn("alice")
	bob := h.signedIn("bob")

	res := alice.api(http.MethodGet, "/api/v1/me/sessions", "")
	current, others := sessionIDs(t, res)

	if res.StatusCode != http.StatusOK || current == "" || len(others) != 1 {
		t.Fatalf("own sessions = %d %q %v", res.StatusCode, current, others)
	}

	// Bob cannot revoke Alice's session (SR-18).
	if res := bob.api(http.MethodDelete, "/api/v1/me/sessions/"+others[0], ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("IDOR = %d", res.StatusCode)
	}

	if res := alice.api(http.MethodDelete, "/api/v1/me/sessions/"+others[0], ""); res.StatusCode != http.StatusNoContent {
		t.Errorf("revoke = %d", res.StatusCode)
	}

	if s := alice2.session(); s["authenticated"] != false {
		t.Error("revoked session still valid")
	}

	// Admin side.
	root := h.signedIn("root")
	aliceID := h.userID("alice")

	res = root.api(http.MethodGet, "/api/v1/users/"+aliceID+"/sessions", "")
	if _, list := sessionIDs(t, res); res.StatusCode != http.StatusOK || len(list) != 1 {
		t.Errorf("admin list = %d %v", res.StatusCode, list)
	}

	if res := bob.api(http.MethodGet, "/api/v1/users/"+aliceID+"/sessions", ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener reads another user's sessions = %d", res.StatusCode)
	}

	res = root.api(http.MethodPost, "/api/v1/users/"+aliceID+"/sessions/revoke", "")
	if v := decode(t, res); res.StatusCode != http.StatusOK || v["revoked"] != float64(1) {
		t.Errorf("revoke all = %d %v", res.StatusCode, v)
	}

	if s := alice.session(); s["authenticated"] != false {
		t.Error("session survived sign-out everywhere")
	}

	// logout-all signs out every own session, this one included.
	b2 := h.signedIn("bob")

	res = bob.api(http.MethodPost, "/api/v1/auth/logout-all", "")
	if res.StatusCode != http.StatusNoContent || setCookie(res, "__Host-rx_session") == nil {
		t.Errorf("logout-all = %d", res.StatusCode)
	}

	if s := b2.session(); s["authenticated"] != false {
		t.Error("other session survived logout-all")
	}
}
