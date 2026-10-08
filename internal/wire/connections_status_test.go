package wire_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	connV4 = "0192f2b4-0000-7000-8000-0000000000a1"
	connV6 = "0192f2b4-0000-7000-8000-0000000000a2"
)

// openConnection inserts an open presence row.
func (h *adminHub) openConnection(id, kind, ip string) {
	h.t.Helper()

	// Serve closes the rows left open by a previous process before it
	// answers: a first request tells that it did.
	h.browser("")

	hex := strings.ReplaceAll(id, "-", "")
	now := time.Now().UnixMilli()
	// The test hub reaps stale rows within milliseconds: heartbeat in the
	// future keeps the row open.
	beat := time.Now().Add(time.Hour).UnixMilli()

	_, err := h.db.Writer(context.Background()).ExecContext(context.Background(), fmt.Sprintf(
		`INSERT INTO connections (id, kind, role_id, ip, opened_at, last_heartbeat_at) VALUES (X'%s', ?, 0, ?, ?, ?)`, hex),
		kind, ip, now, beat)
	if err != nil {
		h.t.Fatal(err)
	}
}

// TestConnectionsMaskAndReveal: PRS-001. Addresses are masked by default,
// "Reveal" is an admin POST that shows one address and is audited.
func TestConnectionsMaskAndReveal(t *testing.T) {
	h := newAdminHub(t, nil)
	h.openConnection(connV4, "media", "192.0.2.77")
	h.openConnection(connV6, "map", "2001:db8:1234:5678::9")

	admin := h.browser("root")

	_, page := admin.do(http.MethodGet, "/admin/connections", "", "", nil)
	body := string(page)

	for _, want := range []string{"192.0.2.x", "2001:db8:1234::/48", "anonymous", "receiver", ">map<", "Preset", "Band", "Since", "/admin/connections/" + connV4 + "/reveal"} {
		if !strings.Contains(body, want) {
			t.Errorf("connections page lacks %q", want)
		}
	}

	for _, leak := range []string{"192.0.2.77", "5678::9"} {
		if strings.Contains(body, leak) {
			t.Errorf("connections page leaks %q", leak)
		}
	}

	// Reveal is for admins.
	for _, user := range []string{"", "lis", "op"} {
		res, _ := h.browser(user).form("/admin/connections/"+connV4+"/reveal", nil, false)
		if res.StatusCode != http.StatusForbidden && !strings.HasPrefix(res.Header.Get("Location"), "/login") {
			t.Errorf("%q reveal = %d %s", user, res.StatusCode, res.Header.Get("Location"))
		}
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action = 'connections.ip.reveal'"); n != 0 {
		t.Fatalf("%d reveal audit rows after refused requests", n)
	}

	// Without the CSRF token.
	token := admin.token
	admin.token = "forged"

	if res, _ := admin.form("/admin/connections/"+connV4+"/reveal", nil, true); res.StatusCode != http.StatusForbidden {
		t.Errorf("reveal without CSRF = %d", res.StatusCode)
	}

	admin.token = token

	// An admin reveals one row: only that address is shown, the reveal is
	// audited with the connection id.
	res, fragment := admin.form("/admin/connections/"+connV4+"/reveal", url.Values{}, true)
	if res.StatusCode != http.StatusOK || !strings.Contains(fragment, "192.0.2.77") || strings.Contains(fragment, "5678::9") ||
		strings.Contains(fragment, "<html") {
		t.Errorf("reveal = %d %s", res.StatusCode, fragment)
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action = 'connections.ip.reveal' AND target_id = ?", connV4); n != 1 {
		t.Errorf("%d reveal audit rows", n)
	}

	// The next load masks again; an unknown connection is a 404 and is not
	// audited.
	if _, again := admin.do(http.MethodGet, "/admin/connections", "", "", nil); strings.Contains(string(again), "192.0.2.77") {
		t.Error("the address is still revealed on the next load")
	}

	if res, _ := admin.form("/admin/connections/0192f2b4-0000-7000-8000-0000000000ff/reveal", url.Values{}, true); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown connection = %d", res.StatusCode)
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action = 'connections.ip.reveal'"); n != 1 {
		t.Errorf("%d reveal audit rows", n)
	}
}

// TestConnectionsUnmasked: with privacy.mask_ips off, addresses are shown
// and there is nothing to reveal.
func TestConnectionsUnmasked(t *testing.T) {
	h := newAdminHub(t, map[string]string{"MESHSDR_SETTINGS__PRIVACY__MASK_IPS": "false"})
	h.openConnection(connV4, "media", "192.0.2.77")

	_, page := h.browser("root").do(http.MethodGet, "/admin/connections", "", "", nil)
	if !strings.Contains(string(page), "192.0.2.77") || strings.Contains(string(page), "/reveal") {
		t.Errorf("unmasked page = %s", page)
	}
}

// TestPublicStatus: API-003 over the real hub: anonymous access, the alias
// and the e-mail gating.
func TestPublicStatus(t *testing.T) {
	for _, tt := range []struct {
		name      string
		env       map[string]string
		wantEmail bool
	}{
		{"e-mail private by default", map[string]string{"MESHSDR_SETTINGS__RECEIVER__ADMIN_EMAIL": "admin@example.org"}, false},
		{"e-mail public", map[string]string{
			"MESHSDR_SETTINGS__RECEIVER__ADMIN_EMAIL": "admin@example.org", "MESHSDR_SETTINGS__RECEIVER__ADMIN_EMAIL_PUBLIC": "true",
			"MESHSDR_SETTINGS__RECEIVER__LOCATION": "Lille", "MESHSDR_SETTINGS__RECEIVER__ALTITUDE_M": "25", "MESHSDR_SETTINGS__RECEIVER__GPS": "50.63,3.06",
		}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newAdminHub(t, tt.env)
			anon := h.browser("")

			code, st, _ := anon.json(http.MethodGet, "/api/v1/status", "")
			if code != http.StatusOK || st["name"] != "MeshSDR" || st["version"] == "" || st["device_count"] != float64(0) {
				t.Fatalf("status = %d %v", code, st)
			}

			if devices, ok := st["devices"].([]any); !ok || len(devices) != 0 {
				t.Errorf("devices = %v", st["devices"])
			}

			if _, has := st["admin_email"]; has != tt.wantEmail {
				t.Errorf("admin_email present = %v, want %v: %v", has, tt.wantEmail, st)
			}

			if _, has := st["altitude"]; has != tt.wantEmail || (tt.wantEmail && st["altitude"] != float64(25)) {
				t.Errorf("altitude = %v", st["altitude"])
			}

			if tt.wantEmail && (st["location"] != "Lille" || st["position"] == nil) {
				t.Errorf("location and position missing: %v", st)
			}

			code, alias, res := anon.json(http.MethodGet, "/status.json", "")
			if code != http.StatusOK || res.Header.Get("Content-Type") != "application/json" || fmt.Sprint(alias) != fmt.Sprint(st) {
				t.Errorf("/status.json = %d %v", code, alias)
			}
		})
	}
}
