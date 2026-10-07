package http_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

func TestAuditPages(t *testing.T) {
	h := newHub(t)
	h.addUser("root", domain.RoleAdmin)
	h.addUser("alice", domain.RoleListener)

	root := h.signedIn("root")
	alice := h.signedIn("alice")

	// A failed login whose typed identifier looks like a formula.
	anon := h.client()
	anon.session()
	anon.login("=cmd|' /C calc'!A0", "x", false)

	if res := alice.do(http.MethodGet, "/admin/audit", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener = %d", res.StatusCode)
	}

	res := root.do(http.MethodGet, "/admin/audit?actor=alice", "", "", nil)
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "auth.login.success") || strings.Contains(b, ">root<") {
		t.Errorf("page = %d %s", res.StatusCode, b)
	}

	res = root.form("/admin/audit/export", url.Values{"format": {"csv"}}, false)
	csv, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/csv") ||
		!strings.HasPrefix(string(csv), "id,at,actor_kind") || !strings.Contains(string(csv), "auth.login.failure") {
		t.Errorf("csv = %d %s", res.StatusCode, csv)
	}

	for _, line := range strings.Split(string(csv), "\n") {
		for _, cell := range strings.Split(line, ",") {
			if strings.HasPrefix(cell, "=") || strings.HasPrefix(cell, "\"=") {
				t.Errorf("formula cell %q", cell)
			}
		}
	}

	res = root.form("/admin/audit/export", url.Values{"format": {"json"}, "action": {"auth.login.success"}}, false)

	var entries []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&entries); err != nil || len(entries) != 2 {
		t.Errorf("json export = %v, %d entries", err, len(entries))
	}

	if res := root.form("/admin/audit/export", url.Values{"format": {"xml"}}, false); res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown format = %d", res.StatusCode)
	}

	if res := alice.form("/admin/audit/export", url.Values{"format": {"csv"}}, false); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener export = %d", res.StatusCode)
	}
}
