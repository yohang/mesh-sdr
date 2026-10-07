package http_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// No log line carries a setup token, whatever the request (routed or not,
// refused by CSRF, by the forced password change or by a role check).
func TestTokensNeverReachTheLogs(t *testing.T) {
	h := newHub(t)
	token := strings.TrimPrefix(h.beginSetup(), "/setup/")
	pw := h.addFlagged("alice", domain.RoleListener)

	anon := h.client()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/setup/" + token},
		{http.MethodGet, "/setup/" + token + "/"},
		{http.MethodGet, "/setup/" + token + "/x"},
		{http.MethodGet, "//setup/" + token},
		{http.MethodGet, "/SETUP/" + token},
		{http.MethodPost, "/setup/" + token},               // 405, CSRF refusal first
		{http.MethodDelete, "/api/v1/auth/setup/" + token}, // no such operation: 404
		{http.MethodPost, "/api/v1/auth/invitations/" + token + "/accept"},
	} {
		anon.do(tc.method, tc.path, "", "", nil)
	}

	flagged := h.client()
	flagged.session()
	flagged.login("alice", pw, false)

	res := flagged.do(http.MethodGet, "/setup/"+token, "", "", nil)
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || strings.Contains(loc, token) || strings.Contains(loc, "next=") {
		t.Errorf("forced-change redirect = %d %q", res.StatusCode, loc)
	}

	flagged.do(http.MethodGet, "/admin/setup/"+token, "", "", nil)

	if logs := h.logs.String(); strings.Contains(logs, token) {
		t.Errorf("a token reached the logs:\n%s", logs)
	} else if !strings.Contains(logs, "/setup/{token}") || !strings.Contains(logs, "csrf check failed") {
		t.Errorf("expected redacted lines are missing:\n%s", logs)
	}
}
