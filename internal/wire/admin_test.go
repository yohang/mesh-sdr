package wire_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/wire"
)

const testPassword = "correct horse battery"

// adminHub is a served hub with an admin, an operator and a listener.
type adminHub struct {
	t    *testing.T
	base string
	db   *db.DB
}

// newAdminHub serves a hub whose config sets env (locked settings).
func newAdminHub(t *testing.T, env map[string]string) *adminHub {
	t.Helper()

	dir := t.TempDir()
	toml := "schema_version = 1\n[hub]\nurl = \"http://127.0.0.1\"\nallow_insecure_url = true\n" +
		"[gateway]\ntls_mode = \"off\"\nhttp_listen = \"127.0.0.1:0\"\nstorage_dir = \"" + filepath.Join(dir, "acme") + "\"\n" +
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

	admin := identity.UserAdmin(wire.IdentityDeps(cfg, discard, a))
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
		form := url.Values{"login": {user}, "password": {testPassword}}
		res, body := b.do(http.MethodPost, "/login", "application/x-www-form-urlencoded", form.Encode(), nil)
		if res.StatusCode != http.StatusSeeOther {
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

func (h *adminHub) count(query string, args ...any) int {
	h.t.Helper()

	var n int
	if err := h.db.Reader(context.Background()).QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}

	return n
}

// TestSettingsAccess: the settings forms and the effective configuration
// are for admins only.
func TestSettingsAccess(t *testing.T) {
	h := newAdminHub(t, nil)

	for _, tt := range []struct {
		user         string
		form, config int
	}{{"", 303, 401}, {"lis", 403, 403}, {"op", 403, 403}, {"root", 200, 200}} {
		b := h.browser(tt.user)

		if status, _, _ := b.json(http.MethodGet, "/api/v1/config/effective", ""); status != tt.config {
			t.Errorf("%q GET /config/effective = %d, want %d", tt.user, status, tt.config)
		}

		if res, _ := b.form("/admin/look-and-feel", url.Values{"section": {"theme"}, "ui.theme_mode": {"dark"}}, true); res.StatusCode != tt.form &&
			(tt.form != 303 || res.StatusCode != http.StatusNoContent) {
			t.Errorf("%q save = %d, want %d", tt.user, res.StatusCode, tt.form)
		}
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

	for path, form := range map[string]url.Values{
		"/admin/look-and-feel": {"section": {"theme"}, "ui.theme_mode": {"dark"}},
		"/admin/site":          {"section": {"station"}, "receiver.name": {"F4XYZ WebSDR"}},
		"/admin/site#policy":   {"section": {"policy"}, "receiver.usage_policy_text": {"# House rules"}},
		"/admin/access":        {"section": {"sessions"}, "session.idle_timeout": {"2h"}},
	} {
		if res, body := b.form(strings.TrimSuffix(path, "#policy"), form, true); res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved.") {
			t.Fatalf("save %v = %d %s", form, res.StatusCode, body)
		}
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

// auth.password_min_length (ADR 0011) is read from the settings store.
func TestPasswordMinLengthSetting(t *testing.T) {
	h := newAdminHub(t, nil)

	if res, body := h.browser("root").form("/admin/access", url.Values{"section": {"passwords"}, "auth.password_min_length": {"30"}}, true); res.StatusCode != http.StatusOK {
		t.Fatalf("save = %d %s", res.StatusCode, body)
	}

	b := h.browser("lis")

	res, body := b.form("/account/password", url.Values{
		"current_password": {testPassword}, "new_password": {"a much longer passphrase"}, "confirm_password": {"a much longer passphrase"},
	}, true)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "30") {
		t.Errorf("24-character password = %d %s", res.StatusCode, body)
	}
}
