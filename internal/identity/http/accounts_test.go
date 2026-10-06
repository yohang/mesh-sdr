package http_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
)

// signedIn returns a client signed in as name (password `password`).
func (h *hub) signedIn(name string) *client {
	h.t.Helper()

	c := h.client()
	c.session()

	if res := c.login(name, password, false); res.StatusCode != http.StatusOK {
		h.t.Fatalf("login %s = %d", name, res.StatusCode)
	}

	c.session()

	return c
}

func (h *hub) userID(name string) string {
	h.t.Helper()

	users, err := h.admin.List(context.Background(), true)
	if err != nil {
		h.t.Fatal(err)
	}

	for _, u := range users {
		if u.Username().String() == name {
			return u.ID().String()
		}
	}

	h.t.Fatalf("no user %s", name)

	return ""
}

func (c *client) api(method, path, body string) *http.Response {
	c.h.t.Helper()

	ctype := ""
	if body != "" {
		ctype = "application/json"
	}

	return c.do(method, path, ctype, body, map[string]string{identityhttp.CSRFHeader: c.token})
}

func TestRolesAPI(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)
	h.addUser("alice", domain.RoleListener)

	alice := h.userID("alice")
	root := h.signedIn("root")
	victim := h.signedIn("alice")

	if res := root.api(http.MethodGet, "/api/v1/roles", ""); res.StatusCode != http.StatusOK || len(decode(t, res)["roles"].([]any)) != 3 {
		t.Errorf("roles = %d", res.StatusCode)
	}

	res := root.api(http.MethodPut, "/api/v1/users/"+alice+"/roles", `{"grants":[{"role":"operator","device_id":"rtl-1"}]}`)
	v := decode(t, res)

	if g, _ := v["grants"].([]any); res.StatusCode != http.StatusOK || len(g) != 1 {
		t.Fatalf("set roles = %d %v\n%s", res.StatusCode, v, h.logs.String())
	}

	if s := victim.session(); s["authenticated"] != false {
		t.Error("the user's session survived a role change")
	}

	if res := root.api(http.MethodGet, "/api/v1/users/"+alice+"/roles", ""); res.StatusCode != http.StatusOK {
		t.Errorf("get roles = %d", res.StatusCode)
	}

	for _, tc := range []struct {
		path, body, code string
		status           int
	}{
		{"/api/v1/users/" + alice + "/roles", `{"grants":[{"role":"admin","device_id":"rtl-1"}]}`, "invalid_role", http.StatusUnprocessableEntity},
		{"/api/v1/users/" + alice + "/roles", `{"grants":[{"role":"operator","device_id":"Bad Device"}]}`, "invalid_device_id", http.StatusUnprocessableEntity},
		{"/api/v1/users/" + alice + "/roles", `{"grants":[],"x":1}`, "unknown_field", http.StatusBadRequest},
		{"/api/v1/users/not-an-id/roles", `{"grants":[]}`, "user_not_found", http.StatusNotFound},
		{"/api/v1/users/" + h.userID("root") + "/roles", `{"grants":[]}`, "last_admin", http.StatusConflict},
	} {
		res := root.api(http.MethodPut, tc.path, tc.body)
		if v := decode(t, res); res.StatusCode != tc.status || v["code"] != tc.code {
			t.Errorf("PUT %s %s = %d %v", tc.path, tc.body, res.StatusCode, v)
		}
	}

	// Rights: admin only.
	h.addUser("op", domain.RoleOperator)

	op := h.signedIn("op")
	if res := op.api(http.MethodGet, "/api/v1/roles", ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("operator = %d", res.StatusCode)
	}

	if res := h.client().api(http.MethodGet, "/api/v1/users/"+alice+"/roles", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", res.StatusCode)
	}

}
