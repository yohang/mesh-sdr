package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/http/api/apitest"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
)

// API-002 contract tests: the whole hub, its REST API validated against
// openapi.yaml (apitest), with one account per role.

const contractPassword = "contract test passphrase"

// clientAddr hands out distinct client addresses (X-Forwarded-For from the
// trusted loopback proxy), so the per-address login limit never trips.
var clientAddr atomic.Int32

type contractHub struct {
	t       *testing.T
	handler http.Handler
	url     string
	adapter db.Adapter
	setup   string
}

// newContractHub serves a hub through v. Admins are allowed from
// 10.0.0.0/8 only; users are created with roles (name → role).
func newContractHub(t *testing.T, v *apitest.Validator, users map[string]identitydomain.Role) *contractHub {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	// The grid needs the hub CA for the node operations.
	certPEM, keyPEM, err := pki.GenerateCA("contract hub CA", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(dir, "tls", "ca.pem"), string(certPEM), 0o644)
	writeFile(t, filepath.Join(dir, "tls", "ca.key"), string(keyPEM), 0o600)
	writeFile(t, filepath.Join(dir, "hub.toml"), `schema_version = 1
[hub]
listen = "127.0.0.1:0"
url = "http://hub.test"
allow_insecure_url = true
[db]
dsn = "sqlite://`+filepath.Join(dir, "hub.db")+`"
[tls]
ca_cert = "tls/ca.pem"
ca_key = { file = "tls/ca.key" }
[auth.argon2]
memory_kib = 19456
iterations = 2
parallelism = 1
[http]
trusted_proxies = ["127.0.0.0/8", "::1/128"]
[admin]
allowed_networks = ["10.0.0.0/8"]
`, 0o600)

	cfg, meta, err := config.LoadHub(config.Options{Dir: dir, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	cfg.Auth.TokenKeyDir = filepath.Join(dir, "keys")

	adapter, err := OpenDB(ctx, cfg.DB, quiet)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = adapter.Close() })

	if _, err := adapter.Migrator().Up(ctx); err != nil {
		t.Fatal(err)
	}

	admin := UserAdmin(cfg, quiet, adapter)
	for name, role := range users {
		if _, err := admin.Add(ctx, identityapp.AddUserInput{Username: name, Role: role, Password: contractPassword}); err != nil {
			t.Fatal(err)
		}
	}

	p, _, err := newHub(ctx, cfg, meta.Origins, quiet, adapter, time.Now, gridapp.DefaultTimings())
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(v.Handler(t, p.server.Handler))
	t.Cleanup(srv.Close)

	return &contractHub{t: t, handler: p.server.Handler, url: srv.URL, adapter: adapter, setup: strings.TrimPrefix(p.SetupURL(), cfg.Hub.URL)}
}

// apiClient is a browser-like API client: cookies, CSRF token, address.
type apiClient struct {
	h    *contractHub
	http *http.Client
	csrf string
	addr string
}

// client returns an anonymous client from a fresh address in network
// (10 or 192).
func (h *contractHub) client(network int) *apiClient {
	h.t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}

	n := clientAddr.Add(1)
	c := &apiClient{h: h, http: &http.Client{Jar: jar}, addr: fmt.Sprintf("%d.0.%d.%d", network, n/250, n%250+1)}

	_, body := c.do(http.MethodGet, "/auth/session", nil)
	c.csrf, _ = body["csrf_token"].(string)

	return c
}

// signedIn returns a client signed in as name.
func (h *contractHub) signedIn(name string, network int) *apiClient {
	h.t.Helper()

	c := h.client(network)

	status, body := c.do(http.MethodPost, "/auth/login", map[string]any{"login": name, "password": contractPassword})
	if status != http.StatusOK {
		h.t.Fatalf("login %s = %d %v", name, status, body)
	}

	c.csrf, _ = body["csrf_token"].(string)

	return c
}

// do sends a request to /api/v1<path>; body is JSON-encoded unless nil.
func (c *apiClient) do(method, path string, body any) (int, map[string]any) {
	c.h.t.Helper()

	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.h.t.Fatal(err)
		}

		r = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, c.h.url+apitest.Prefix+path, r)
	if err != nil {
		c.h.t.Fatal(err)
	}

	req.Header.Set("X-Forwarded-For", c.addr)
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}

	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}

	defer func() { _ = res.Body.Close() }()

	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)

	return res.StatusCode, out
}

// raw sends a request with a body of the given content type.
func (c *apiClient) raw(method, path, ctype string, body []byte) int {
	c.h.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), method, c.h.url+apitest.Prefix+path, bytes.NewReader(body))
	if err != nil {
		c.h.t.Fatal(err)
	}

	req.Header.Set("X-Forwarded-For", c.addr)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("X-CSRF-Token", c.csrf)

	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}

	_ = res.Body.Close()

	return res.StatusCode
}

// avatar returns a multipart body with a small PNG in its file part.
func avatar(t *testing.T) (string, []byte) {
	t.Helper()

	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer

	w := multipart.NewWriter(&body)

	part, err := w.CreateFormFile("file", "avatar.png")
	if err != nil {
		t.Fatal(err)
	}

	_, _ = part.Write(img.Bytes())

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return w.FormDataContentType(), body.Bytes()
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestAPIContract covers API-002 end to end: every operation of
// openapi.yaml, called by every role, enforces its x-meshsdr-access level
// (and the admin network); every exchange matches the document; every
// operation answers a validated 2xx at least once.
func TestAPIContract(t *testing.T) {
	v, err := apitest.New(api.Spec())
	if err != nil {
		t.Fatal(err)
	}

	roles := map[string]identitydomain.Role{
		"listener": identitydomain.RoleListener, "operator": identitydomain.RoleOperator, "admin": identitydomain.RoleAdmin,
		"changer": identitydomain.RoleListener, "leaver": identitydomain.RoleListener, "victim": identitydomain.RoleListener,
	}
	h := newContractHub(t, v, roles)

	t.Run("rights", func(t *testing.T) { checkRights(t, h, v) })
	t.Run("happy paths", func(t *testing.T) { happyPaths(t, h) })
	t.Run("first admin setup", func(t *testing.T) {
		fresh := newContractHub(t, v, nil)

		c := fresh.client(10)
		token := strings.TrimPrefix(fresh.setup, "/setup/")

		if status, body := c.do(http.MethodGet, "/auth/setup/"+token, nil); status != http.StatusOK {
			t.Errorf("checkSetup = %d %v", status, body)
		}

		status, body := c.do(http.MethodPost, "/auth/setup", map[string]any{"token": token, "username": "root", "password": contractPassword})
		if status != http.StatusOK {
			t.Errorf("completeSetup = %d %v", status, body)
		}
	})

	// The probe needs a connected node: TestGridEndToEnd covers it. The
	// e-mail confirmation and the test e-mail need mail, which this hub
	// does not configure: the identity HTTP tests cover them.
	exempt := map[string]bool{"probeNodeCapabilities": true, "confirmEmail": true, "sendTestMail": true}

	for _, id := range v.Uncovered() {
		if !exempt[id] {
			t.Errorf("operation %s never answered a validated 2xx", id)
		}
	}
}

// checkRights calls every operation as each role (bodies carry a
// placeholder for each required field, ids are placeholders): below the
// required level the policy refuses it (401 unauthenticated, 403 forbidden
// or admin_network_denied), at or above it never does.
func checkRights(t *testing.T, h *contractHub, v *apitest.Validator) {
	type caller struct {
		name    string
		role    identitydomain.Role
		account string
		network int
	}

	callers := []caller{
		{"anonymous", identitydomain.RoleAnonymous, "", 10},
		{"anonymous outside admin networks", identitydomain.RoleAnonymous, "", 192},
		{"listener", identitydomain.RoleListener, "listener", 10},
		{"operator", identitydomain.RoleOperator, "operator", 10},
		{"admin", identitydomain.RoleAdmin, "admin", 10},
		{"admin outside admin networks", identitydomain.RoleAdmin, "admin", 192},
	}

	// denial are the policy's refusals. An allowed call must get none of
	// them, nor a CSRF failure (which would hide the policy's answer); a
	// refused call must get one of them, not a CSRF failure.
	denial := map[string]bool{"unauthenticated": true, "forbidden": true, "admin_network_denied": true}

	// The first-admin setup is anonymous but creates an admin: it is
	// restricted to admin.allowed_networks too (AUTH-018).
	adminNetworkOnly := map[string]bool{"checkSetup": true, "completeSetup": true}

	for _, op := range v.Operations() {
		need, err := identitydomain.ParseRole(op.Access)
		if err != nil {
			t.Errorf("%s: x-meshsdr-access %q: %v", op.ID, op.Access, err)

			continue
		}

		path := pathParam.ReplaceAllString(op.Path, "contract-x")

		var body any
		if op.HasBody {
			body = map[string]any{}
			if op.MinimalBody != nil {
				body = op.MinimalBody
			}
		}

		for _, c := range callers {
			// A fresh session per call: an operation may end it (logout).
			client := h.client(c.network)
			if c.account != "" {
				client = h.signedIn(c.account, c.network)
			}

			status, res := client.do(op.Method, path, body)
			code, _ := res["code"].(string)

			allowed := c.role.Includes(need) && ((need != identitydomain.RoleAdmin && !adminNetworkOnly[op.ID]) || c.network == 10)

			switch {
			case allowed && (denial[code] || code == "csrf_failed"):
				t.Errorf("%s %s as %s: refused %d %s", op.Method, op.Path, c.name, status, code)
			case !allowed && adminNetworkOnly[op.ID]:
				if status != http.StatusForbidden || code != "admin_network_denied" {
					t.Errorf("%s %s as %s: %d %s, want 403 admin_network_denied", op.Method, op.Path, c.name, status, code)
				}
			case !allowed && c.role == identitydomain.RoleAnonymous && (status != http.StatusUnauthorized || code != "unauthenticated"):
				t.Errorf("%s %s as %s: %d %s, want 401 unauthenticated", op.Method, op.Path, c.name, status, code)
			case !allowed && c.role != identitydomain.RoleAnonymous && (status != http.StatusForbidden || !denial[code]):
				t.Errorf("%s %s as %s: %d %s, want 403", op.Method, op.Path, c.name, status, code)
			}
		}
	}
}

// happyPaths runs each operation once the way a client uses it, so that
// every one answers a validated 2xx.
func happyPaths(t *testing.T, h *contractHub) {
	anon := h.client(10)
	admin := h.signedIn("admin", 10)
	ctx := context.Background()

	expect := func(c *apiClient, method, path string, body any, want int) map[string]any {
		t.Helper()

		status, res := c.do(method, path, body)
		if status != want {
			t.Errorf("%s %s = %d %v, want %d", method, path, status, res, want)
		}

		return res
	}

	for _, p := range []string{"/openapi.json", "/healthz/live", "/healthz/ready", "/auth/session", "/connections", "/features"} {
		expect(anon, http.MethodGet, p, nil, http.StatusOK)
	}

	// Nodes, their capabilities and devices.
	expect(admin, http.MethodPost, "/nodes", map[string]any{"id": "attic", "url": "https://10.8.0.12:8074"}, http.StatusCreated)
	expect(admin, http.MethodGet, "/nodes", nil, http.StatusOK)
	detail := expect(admin, http.MethodGet, "/nodes/attic", nil, http.StatusOK)

	node, _ := detail["node"].(map[string]any)
	version, _ := node["version"].(float64)
	expect(admin, http.MethodPatch, "/nodes/attic", map[string]any{"version": version, "name": "Attic"}, http.StatusOK)
	expect(admin, http.MethodPost, "/nodes/attic/enrollment-token", nil, http.StatusOK)

	now := time.Now()

	ft8, err := domain.NewCapability("mode:ft8", true, domain.CapabilityOK, "", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}

	report, err := domain.NewCapabilityReport(domain.MustNodeID("attic"), "h", "1.0.0", []string{"rx-ctl.v1"},
		json.RawMessage(`{}`), json.RawMessage(`{}`), now, []domain.Capability{ft8})
	if err != nil {
		t.Fatal(err)
	}

	if err := gridsqlite.NewCapabilityRepository(h.adapter).Replace(ctx, report); err != nil {
		t.Fatal(err)
	}

	dev, err := domain.NewReportedDevice(domain.MustNodeID("attic"), domain.DeviceSpec{
		ID: domain.MustDeviceID("hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 100_000, FreqMax: 30_000_000,
		SampleRates: []int64{2_048_000},
	}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := gridsqlite.NewDeviceRepository(h.adapter).Save(ctx, dev); err != nil {
		t.Fatal(err)
	}

	expect(admin, http.MethodGet, "/nodes/attic/capabilities", nil, http.StatusOK)
	expect(admin, http.MethodGet, "/devices", nil, http.StatusOK)
	expect(admin, http.MethodGet, "/devices/hf", nil, http.StatusOK)
	expect(admin, http.MethodPost, "/auth/token", map[string]any{"node_id": "attic", "cid": "c1"}, http.StatusOK)

	// A device its node no longer reports can be forgotten.
	dev.MarkUnavailable(now)

	if err := gridsqlite.NewDeviceRepository(h.adapter).Save(ctx, dev); err != nil {
		t.Fatal(err)
	}

	features := expect(anon, http.MethodGet, "/features", nil, http.StatusOK)
	if devices, _ := features["devices"].([]any); len(devices) != 1 {
		t.Errorf("features = %v", features)
	}

	expect(admin, http.MethodDelete, "/devices/hf", nil, http.StatusNoContent)
	expect(admin, http.MethodDelete, "/nodes/attic", nil, http.StatusNoContent)

	// Settings, retention and receiver images (ADR 0010).
	for _, p := range []string{"/settings", "/settings/schema", "/settings/public", "/config/effective", "/retention"} {
		expect(admin, http.MethodGet, p, nil, http.StatusOK)
	}

	expect(admin, http.MethodPatch, "/settings", map[string]any{
		"values": map[string]any{"receiver.location": "Lille"}, "versions": map[string]any{"receiver.location": 0},
	}, http.StatusOK)
	expect(admin, http.MethodDelete, "/settings/receiver.location", nil, http.StatusNoContent)
	expect(admin, http.MethodPost, "/retention/sessions/purge", nil, http.StatusOK)

	ctype, img := avatar(t)
	if status := admin.raw(http.MethodPut, "/branding/avatar", ctype, img); status != http.StatusOK {
		t.Errorf("PUT /branding/avatar = %d", status)
	}

	if status, _ := anon.do(http.MethodGet, "/branding/avatar", nil); status != http.StatusOK {
		t.Errorf("GET /branding/avatar = %d", status)
	}

	expect(admin, http.MethodDelete, "/branding/avatar", nil, http.StatusNoContent)

	// Password change.
	changer := h.signedIn("changer", 10)
	expect(changer, http.MethodPost, "/auth/password", map[string]any{"current_password": contractPassword, "new_password": "another contract passphrase"}, http.StatusOK)

	accountPaths(t, h, admin, expect)

	expect(admin, http.MethodPost, "/auth/logout", nil, http.StatusNoContent)
}

// accountPaths runs the account, user, invitation and password reset
// operations (ADR 0011) the way a client uses them.
func accountPaths(t *testing.T, h *contractHub, admin *apiClient, expect func(*apiClient, string, string, any, int) map[string]any) {
	t.Helper()

	anon := h.client(10)

	// The signing keys stay outside /api, at their well-known path.
	res, err := http.Get(h.url + "/.well-known/jwks.json")
	if err != nil {
		t.Fatal(err)
	}

	_ = res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /.well-known/jwks.json = %d", res.StatusCode)
	}

	// The caller's own account: e-mail (applied at once without mail),
	// sessions, deletion.
	leaver := h.signedIn("leaver", 10)
	other := h.signedIn("leaver", 10)

	expect(leaver, http.MethodPost, "/me/email", map[string]any{"email": "leaver@example.org", "current_password": contractPassword}, http.StatusOK)

	sessions := expect(leaver, http.MethodGet, "/me/sessions", nil, http.StatusOK)
	list, _ := sessions["sessions"].([]any)

	revoked := 0

	for _, s := range list {
		if s, _ := s.(map[string]any); s["current"] == false {
			expect(leaver, http.MethodDelete, "/me/sessions/"+s["id"].(string), nil, http.StatusNoContent)

			revoked++
		}
	}

	if status, _ := other.do(http.MethodGet, "/me", nil); revoked != 1 || status != http.StatusUnauthorized {
		t.Errorf("revoked %d other session(s); the other one answers %d", revoked, status)
	}

	expect(leaver, http.MethodDelete, "/me", map[string]any{"current_password": contractPassword}, http.StatusNoContent)

	// User administration.
	id := userID(t, expect(admin, http.MethodGet, "/users", nil, http.StatusOK), "victim")
	user := "/users/" + id

	expect(admin, http.MethodGet, user, nil, http.StatusOK)
	expect(admin, http.MethodGet, user+"/roles", nil, http.StatusOK)
	expect(admin, http.MethodPut, user+"/roles", map[string]any{"grants": []any{map[string]any{"role": "operator", "device_id": "hf"}}}, http.StatusOK)
	expect(admin, http.MethodPatch, user, map[string]any{"display_name": "Victim"}, http.StatusOK)

	// A role change signs the user out: sign in after it.
	victim := h.signedIn("victim", 10)

	userSessions := expect(admin, http.MethodGet, user+"/sessions", nil, http.StatusOK)
	if list, _ := userSessions["sessions"].([]any); len(list) != 1 {
		t.Errorf("user sessions = %v", userSessions)
	} else {
		ref, _ := list[0].(map[string]any)["id"].(string)
		expect(admin, http.MethodDelete, user+"/sessions/"+ref, nil, http.StatusNoContent)
	}

	if status, _ := victim.do(http.MethodGet, "/me", nil); status != http.StatusUnauthorized {
		t.Errorf("revoked session answers %d", status)
	}

	expect(admin, http.MethodPost, user+"/sessions/revoke", nil, http.StatusOK)
	expect(admin, http.MethodPost, user+"/export", nil, http.StatusOK)
	expect(admin, http.MethodPost, user+"/password", nil, http.StatusOK)

	// A password reset link, shown to copy without mail.
	reset := expect(admin, http.MethodPost, user+"/password-reset", nil, http.StatusOK)
	link, _ := reset["link"].(string)
	_, token, _ := strings.Cut(link, "/password/reset/")
	expect(anon, http.MethodPost, "/auth/password-reset/confirm", map[string]any{"token": token, "new_password": "a reset contract passphrase"}, http.StatusNoContent)

	expect(admin, http.MethodPatch, user, map[string]any{"enabled": false}, http.StatusOK)
	expect(admin, http.MethodDelete, user, nil, http.StatusNoContent)

	// Invitations: one accepted, one revoked.
	created := expect(admin, http.MethodPost, "/invitations", map[string]any{"role": "listener"}, http.StatusCreated)
	link, _ = created["link"].(string)
	_, token, _ = strings.Cut(link, "/invite/")

	invitee := h.client(10)
	expect(invitee, http.MethodGet, "/auth/invitations/"+token, nil, http.StatusOK)
	expect(invitee, http.MethodPost, "/auth/invitations/"+token+"/accept", map[string]any{"username": "invitee", "password": contractPassword}, http.StatusCreated)

	created = expect(admin, http.MethodPost, "/invitations", map[string]any{"role": "listener"}, http.StatusCreated)
	inv, _ := created["invitation"].(map[string]any)
	invID, _ := inv["id"].(string)
	expect(admin, http.MethodDelete, "/invitations/"+invID, nil, http.StatusNoContent)
}

// userID finds the id of username in a user list.
func userID(t *testing.T, list map[string]any, username string) string {
	t.Helper()

	users, _ := list["users"].([]any)
	for _, u := range users {
		if u, _ := u.(map[string]any); u["username"] == username {
			id, _ := u["id"].(string)

			return id
		}
	}

	t.Fatalf("user %s not in %v", username, list)

	return ""
}

// htmlActions maps every state-changing HTML route (htmx forms, ADR 0003
// page-scoped actions) to the /api/v1 operation doing the same, since every
// browser operation must be available in the API (API-002).
var htmlActions = map[string]string{
	"POST /login":            "login",
	"POST /logout":           "logout",
	"POST /account/password": "changePassword",
	"POST /setup":            "completeSetup",
	// Admin pages (ADR 0010): each section form saves settings.
	"POST /admin/site":                "patchSettings",
	"POST /admin/access":              "patchSettings",
	"POST /admin/look-and-feel":       "patchSettings",
	"POST /admin/retention":           "patchSettings",
	"POST /admin/retention/purge":     "purgeStore",
	"POST /admin/site/images":         "putReceiverImage",
	"POST /admin/site/images/remove":  "deleteReceiverImage",
	"POST /admin/devices/{id}/forget": "forgetDevice",
	// Account (ADR 0011).
	"POST /account/profile":               "updateMe",
	"POST /account/email":                 "changeMyEmail",
	"POST /account/email/verify":          "confirmEmail",
	"POST /account/export":                "exportMe",
	"POST /account/delete":                "deleteMe",
	"POST /account/sessions/{ref}/revoke": "revokeOwnSession",
	// Not one-to-one: revokeOwnSession for each session of GET /me/sessions
	// except the current one (ADR 0013).
	"POST /account/sessions/revoke-others": "revokeOwnSession",
	"POST /password/forgot":                "requestPasswordReset",
	"POST /password/reset":                 "confirmPasswordReset",
	"POST /invite":                         "acceptInvitation",
	// Admin › Users, Invitations, Audit.
	"POST /admin/users/{id}/roles":                 "setUserRoles",
	"POST /admin/users/{id}/enable":                "updateUser",
	"POST /admin/users/{id}/disable":               "updateUser",
	"POST /admin/users/{id}/password":              "setGeneratedPassword",
	"POST /admin/users/{id}/password-reset":        "issuePasswordReset",
	"POST /admin/users/{id}/export":                "exportUser",
	"POST /admin/users/{id}/delete":                "deleteUser",
	"POST /admin/users/{id}/sessions/revoke":       "revokeUserSessions",
	"POST /admin/users/{id}/sessions/{ref}/revoke": "revokeUserSession",
	"POST /admin/invitations":                      "createInvitation",
	"POST /admin/invitations/{id}/revoke":          "revokeInvitation",
	"POST /admin/invitations/test-mail":            "sendTestMail",
	// Not one-to-one: the CSV or JSON export formats the results of
	// searchAudit (ADR 0013).
	"POST /admin/audit/export": "searchAudit",
}

// TestHTMLActionsHaveAPITwins walks the hub router: a new unsafe HTML
// route fails until it is mapped to its API operation above.
func TestHTMLActionsHaveAPITwins(t *testing.T) {
	v, err := apitest.New(api.Spec())
	if err != nil {
		t.Fatal(err)
	}

	ops := map[string]bool{}
	for _, op := range v.Operations() {
		ops[op.ID] = true
	}

	h := newContractHub(t, v, nil)

	routes, ok := h.handler.(chi.Routes)
	if !ok {
		t.Fatalf("hub handler %T is not a chi router", h.handler)
	}

	seen := map[string]bool{}

	err = chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return nil
		}

		// The API itself, and the static files (registered for every
		// method, answering only reads).
		if route == "/api" || strings.HasPrefix(route, "/api/") || route == "/static/*" {
			return nil
		}

		key := method + " " + route
		seen[key] = true

		switch op, mapped := htmlActions[key]; {
		case !mapped:
			t.Errorf("%s has no /api/v1 twin in htmlActions", key)
		case !ops[op]:
			t.Errorf("%s maps to unknown operation %s", key, op)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for key := range htmlActions {
		if !seen[key] {
			t.Errorf("htmlActions lists %s, which the router does not serve", key)
		}
	}
}
