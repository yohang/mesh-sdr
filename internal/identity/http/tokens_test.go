package http_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func TestAccessTokens(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleListener)

	anon := h.client()

	res := anon.do(http.MethodGet, "/.well-known/jwks.json", "", "", nil)

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Crv string `json:"crv"`
		} `json:"keys"`
		RevokedKids []string `json:"revoked_kids"`
	}

	if err := json.NewDecoder(res.Body).Decode(&jwks); err != nil || res.StatusCode != http.StatusOK || len(jwks.Keys) != 1 || jwks.Keys[0].Crv != "Ed25519" {
		t.Fatalf("jwks = %d %+v %v", res.StatusCode, jwks, err)
	}

	anon.session()

	if res := anon.api(http.MethodPost, "/api/v1/auth/token", `{"node_id":"roof","cid":"c1"}`); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", res.StatusCode)
	}

	alice := h.signedIn("alice")

	res = alice.api(http.MethodPost, "/api/v1/auth/token", `{"node_id":"roof","cid":"c1"}`)
	if v := decode(t, res); res.StatusCode != http.StatusOK || v["token"] == "" || v["expires_at"] == nil || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("mint = %d %v", res.StatusCode, v)
	}

	if res := alice.api(http.MethodPost, "/api/v1/auth/token", `{"node_id":"cellar","cid":"c1"}`); res.StatusCode != http.StatusForbidden {
		t.Errorf("node without devices = %d", res.StatusCode)
	}

	if res := alice.do(http.MethodPost, "/api/v1/auth/token", "application/json", `{"node_id":"roof","cid":"c1"}`, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("without CSRF = %d", res.StatusCode)
	}
}
