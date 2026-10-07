package wire_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/wire"
)

const testPassword = "correct horse battery"

// adminHub is a served hub with an admin, an operator and a listener.
type adminHub struct {
	t    *testing.T
	base string
	db   db.Adapter
}

// newAdminHub serves a hub whose config sets env (locked settings).
func newAdminHub(t *testing.T, env map[string]string) *adminHub {
	t.Helper()

	if !wire.GatewayAvailable() {
		t.Skip("the hub needs the gateway (nogateway build)")
	}

	dir := t.TempDir()
	toml := "schema_version = 1\n[hub]\nurl = \"http://127.0.0.1\"\nallow_insecure_url = true\n" +
		"[gateway]\ntls_mode = \"off\"\nhttp_listen = \"127.0.0.1:0\"\nstorage_dir = \"" + filepath.Join(dir, "caddy") + "\"\n" +
		"[auth.argon2]\nmemory_kib = 19456\niterations = 2\nparallelism = 1\n"

	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, meta, err := config.LoadHub(config.Options{Dir: dir, Env: env})
	if err != nil {
		t.Fatal(err)
	}

	cfg.Auth.Argon2 = config.Argon2{MemoryKiB: 64, Iterations: 1, Parallelism: 1}
	cfg.Auth.TokenKeyDir = filepath.Join(t.TempDir(), "keys")
	a := dbtest.NewSQLite(t)

	admin := wire.UserAdmin(cfg, discard, a)
	for name, role := range map[string]domain.Role{"root": domain.RoleAdmin, "op": domain.RoleOperator, "lis": domain.RoleListener} {
		if _, err := admin.Add(context.Background(), identityapp.AddUserInput{Username: name, Role: role, Password: testPassword}); err != nil {
			t.Fatal(err)
		}
	}

	p, err := wire.Hub(context.Background(), cfg, meta.Origins, discard, a)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	return &adminHub{t: t, base: "http://" + serveOn(t, p, ln), db: a}
}

// browser keeps cookies and the CSRF token of one visitor.
type browser struct {
	h     *adminHub
	c     *http.Client
	token string
}

func (h *adminHub) browser(user string) *browser {
	h.t.Helper()

	jar, _ := cookiejar.New(nil)
	b := &browser{h: h, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	b.refreshToken()

	if user != "" {
		res, body := b.do(http.MethodPost, "/api/v1/auth/login", "application/json", `{"login":"`+user+`","password":"`+testPassword+`"}`, nil)
		if res.StatusCode != http.StatusOK {
			h.t.Fatalf("login %s = %d %s", user, res.StatusCode, body)
		}

		b.refreshToken()
	}

	return b
}

func (b *browser) refreshToken() {
	b.h.t.Helper()

	_, body := b.do(http.MethodGet, "/api/v1/auth/session", "", "", nil)

	var s struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		b.h.t.Fatal(err)
	}

	b.token = s.Token
}

func (b *browser) do(method, path, ctype, body string, header map[string]string) (*http.Response, []byte) {
	b.h.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), method, b.h.base+path, strings.NewReader(body))
	if err != nil {
		b.h.t.Fatal(err)
	}

	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}

	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-CSRF-Token", b.token)
		req.Header.Set("Origin", b.h.base)
	}

	for k, v := range header {
		req.Header.Set(k, v)
	}

	res, err := b.c.Do(req)
	if err != nil {
		b.h.t.Fatal(err)
	}

	defer func() { _ = res.Body.Close() }()

	out, err := io.ReadAll(res.Body)
	if err != nil {
		b.h.t.Fatal(err)
	}

	return res, out
}

func (b *browser) json(method, path, body string) (int, map[string]any, *http.Response) {
	b.h.t.Helper()

	ctype := ""
	if body != "" {
		ctype = "application/json"
	}

	res, raw := b.do(method, path, ctype, body, nil)

	var v map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &v); err != nil {
			b.h.t.Fatalf("%s %s: invalid JSON %s", method, path, raw)
		}
	}

	return res.StatusCode, v, res
}

// setting returns the entry of key in a settings list.
func setting(t *testing.T, list map[string]any, key string) map[string]any {
	t.Helper()

	items, _ := list["settings"].([]any)
	for _, it := range items {
		if m, _ := it.(map[string]any); m["key"] == key {
			return m
		}
	}

	t.Fatalf("no setting %s in %v", key, list)

	return nil
}

func (h *adminHub) count(query string, args ...any) int {
	h.t.Helper()

	var n int
	if err := h.db.Reader(context.Background()).QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}

	return n
}

func TestSettingsAPIAccess(t *testing.T) {
	h := newAdminHub(t, nil)

	for _, tt := range []struct {
		user   string
		status int
	}{{"", 401}, {"lis", 403}, {"op", 403}, {"root", 200}} {
		b := h.browser(tt.user)

		for _, path := range []string{"/api/v1/settings", "/api/v1/settings/schema", "/api/v1/config/effective"} {
			if status, _, _ := b.json(http.MethodGet, path, ""); status != tt.status {
				t.Errorf("%q GET %s = %d, want %d", tt.user, path, status, tt.status)
			}
		}

		if status, _, _ := b.json(http.MethodPatch, "/api/v1/settings", `{"values":{"ui.theme_mode":"dark"}}`); status != tt.status {
			t.Errorf("%q PATCH = %d, want %d", tt.user, status, tt.status)
		}

		if status, v, _ := b.json(http.MethodGet, "/api/v1/settings/public", ""); status != 200 || v["receiver.name"] != "MeshSDR" || v["receiver.admin_email"] != nil {
			t.Errorf("%q public settings = %d %v", tt.user, status, v)
		}
	}
}

func TestSettingsAPISaveCycle(t *testing.T) {
	h := newAdminHub(t, map[string]string{"MESHSDR_SETTINGS__UI__SHORTCUT_SET": "off"})
	b := h.browser("root")

	status, list, res := b.json(http.MethodGet, "/api/v1/settings", "")
	if status != 200 || res.Header.Get("ETag") != `"0"` {
		t.Fatalf("GET = %d etag %q", status, res.Header.Get("ETag"))
	}

	if s := setting(t, list, "ui.theme_mode"); s["source"] != "default" || s["value"] != "auto" || s["locked"] != false || s["version"] != 0.0 {
		t.Errorf("theme = %v", s)
	}

	if s := setting(t, list, "ui.shortcut_set"); s["source"] != "cfg" || s["locked"] != true || s["origin"] != "env:MESHSDR_SETTINGS__UI__SHORTCUT_SET" {
		t.Errorf("shortcut set = %v", s)
	}

	// Save.
	status, list, res = b.json(http.MethodPatch, "/api/v1/settings", `{"values":{"ui.theme_mode":"dark","retention.audit_log":"400d"},"versions":{"ui.theme_mode":0}}`)
	if status != 200 || res.Header.Get("ETag") != `"1"` {
		t.Fatalf("PATCH = %d %v", status, list)
	}

	if s := setting(t, list, "ui.theme_mode"); s["source"] != "db" || s["value"] != "dark" || s["version"] != 1.0 {
		t.Errorf("theme after save = %v", s)
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action = 'settings.update' AND result = 'ok' AND target_id = 'ui.theme_mode'"); n != 1 {
		t.Errorf("audit rows = %d", n)
	}

	// Stale version.
	status, p, _ := b.json(http.MethodPatch, "/api/v1/settings", `{"values":{"ui.theme_mode":"light"},"versions":{"ui.theme_mode":0}}`)
	if status != 409 || p["code"] != "version_conflict" {
		t.Errorf("stale = %d %v", status, p)
	}

	// Per-field errors.
	status, p, _ = b.json(http.MethodPatch, "/api/v1/settings", `{"values":{"ui.theme_mode":"sepia","ui.tuning_precision":9,"nope":1}}`)

	errs, _ := p["errors"].([]any)
	if status != 422 || p["code"] != "invalid_setting" || len(errs) != 3 {
		t.Errorf("invalid = %d %v", status, p)
	}

	// Locked key: 409 with the origin, audited as denied.
	status, p, _ = b.json(http.MethodPatch, "/api/v1/settings", `{"values":{"ui.shortcut_set":"default"}}`)
	if status != 409 || p["code"] != "setting_locked" || !strings.Contains(p["detail"].(string), "env:MESHSDR_SETTINGS__UI__SHORTCUT_SET") {
		t.Errorf("locked = %d %v", status, p)
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE result = 'denied' AND target_id = 'ui.shortcut_set'"); n != 1 {
		t.Errorf("denied audit rows = %d", n)
	}

	// null resets; DELETE resets too.
	if status, list, _ = b.json(http.MethodPatch, "/api/v1/settings", `{"values":{"ui.theme_mode":null},"versions":{"ui.theme_mode":1}}`); status != 200 {
		t.Fatalf("reset = %d %v", status, list)
	}

	if s := setting(t, list, "ui.theme_mode"); s["source"] != "default" || s["value"] != "auto" {
		t.Errorf("theme after reset = %v", s)
	}

	if status, p, _ = b.json(http.MethodDelete, "/api/v1/settings/retention.audit_log", ""); status != 204 {
		t.Errorf("DELETE = %d %v", status, p)
	}

	if status, p, _ = b.json(http.MethodDelete, "/api/v1/settings/ui.shortcut_set", ""); status != 409 {
		t.Errorf("DELETE locked = %d %v", status, p)
	}

	if n := h.count("SELECT count(*) FROM settings"); n != 0 {
		t.Errorf("settings rows = %d", n)
	}
}

func TestEffectiveConfigAPI(t *testing.T) {
	h := newAdminHub(t, map[string]string{"MESHSDR_SETTINGS__UI__THEME_MODE": "dark"})
	b := h.browser("root")

	res, raw := b.do(http.MethodGet, "/api/v1/config/effective?download=true", "", "", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("GET = %d %v", res.StatusCode, res.Header)
	}

	var doc struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	byKey := map[string]map[string]any{}
	for _, e := range doc.Entries {
		byKey[e["key"].(string)] = e
	}

	if e := byKey["hub.url"]; e["class"] != "cfg" || e["origin"] != "hub.toml" || e["locked"] != true || e["value"] != "http://127.0.0.1" {
		t.Errorf("hub.url = %v", e)
	}

	if e := byKey["settings.ui.theme_mode"]; e["class"] != "db" || e["source"] != "cfg" || e["value"] != "dark" {
		t.Errorf("theme = %v", e)
	}

	if e := byKey["db.max_read_connections"]; e["source"] != "default" || e["locked"] != false {
		t.Errorf("db.max_read_connections = %v", e)
	}
}

// A saved setting applies at once to its consumers (UI-001, ADR 0010).
func TestSettingsApplyLive(t *testing.T) {
	h := newAdminHub(t, nil)
	b := h.browser("root")

	body := `{"values":{"ui.theme_mode":"dark","receiver.name":"F4XYZ WebSDR","receiver.usage_policy_text":"# House rules","session.idle_timeout":"2h"}}`
	if status, p, _ := b.json(http.MethodPatch, "/api/v1/settings", body); status != 200 {
		t.Fatalf("PATCH = %d %v", status, p)
	}

	anon := h.browser("")

	_, page := anon.do(http.MethodGet, "/", "", "", nil)
	for _, want := range []string{`data-theme="dark"`, `<title>F4XYZ WebSDR</title>`} {
		if !strings.Contains(string(page), want) {
			t.Errorf("home page misses %s", want)
		}
	}

	if _, policy := anon.do(http.MethodGet, "/policy", "", "", nil); !strings.Contains(string(policy), "House rules") {
		t.Error("usage policy not applied")
	}

	// A new session gets the new idle timeout.
	h.browser("op")

	var idle, abs int64
	if err := h.db.Reader(context.Background()).QueryRowContext(context.Background(),
		"SELECT idle_expires_at - last_seen_at, absolute_expires_at - created_at FROM sessions ORDER BY created_at DESC LIMIT 1").Scan(&idle, &abs); err != nil {
		t.Fatal(err)
	}

	if idle != (2*time.Hour).Milliseconds() || abs != (24*time.Hour).Milliseconds() {
		t.Errorf("session lifetimes = %d ms idle, %d ms absolute", idle, abs)
	}
}

func TestRetentionAPI(t *testing.T) {
	h := newAdminHub(t, nil)

	if status, _, _ := h.browser("op").json(http.MethodGet, "/api/v1/retention", ""); status != 403 {
		t.Errorf("operator GET = %d", status)
	}

	b := h.browser("root")

	status, v, _ := b.json(http.MethodGet, "/api/v1/retention", "")
	stores, _ := v["stores"].([]any)

	if status != 200 || len(stores) != 4 {
		t.Fatalf("GET = %d %v", status, v)
	}

	audit, _ := stores[1].(map[string]any)
	if audit["store"] != "audit_log" || audit["retention"] != "365d" || audit["setting_key"] != "retention.audit_log" {
		t.Errorf("audit store = %v", audit)
	}

	// The reporting outbox (ADR 0020).
	outbox, _ := stores[2].(map[string]any)
	if outbox["store"] != "reporting_outbox" || outbox["retention"] != "7d" || outbox["setting_key"] != "retention.reporting_outbox.sent" {
		t.Errorf("outbox store = %v", outbox)
	}

	if status, v, _ = b.json(http.MethodPost, "/api/v1/retention/audit_log/purge", ""); status != 200 || v["rows_deleted"] != 0.0 {
		t.Errorf("purge = %d %v", status, v)
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action = 'retention.purge' AND target_id = 'audit_log'"); n != 1 {
		t.Errorf("purge audit rows = %d", n)
	}

	if n := h.count("SELECT count(*) FROM job_runs WHERE job = 'audit.purge' AND last_status = 'ok'"); n != 1 {
		t.Errorf("job runs = %d", n)
	}

	if status, v, _ = b.json(http.MethodPost, "/api/v1/retention/files/purge", ""); status != 404 || v["code"] != "unknown_store" {
		t.Errorf("unknown store = %d %v", status, v)
	}
}

// auth.password_min_length (ADR 0011) is read from the settings store.
func TestPasswordMinLengthSetting(t *testing.T) {
	h := newAdminHub(t, nil)

	if status, p, _ := h.browser("root").json(http.MethodPatch, "/api/v1/settings", `{"values":{"auth.password_min_length":30}}`); status != 200 {
		t.Fatalf("PATCH = %d %v", status, p)
	}

	b := h.browser("lis")

	status, p, _ := b.json(http.MethodPost, "/api/v1/auth/password", `{"current_password":"`+testPassword+`","new_password":"a much longer passphrase"}`)
	if status != 422 || !strings.Contains(fmt.Sprint(p["errors"]), "too_short") {
		t.Errorf("24-character password = %d %v", status, p)
	}
}
