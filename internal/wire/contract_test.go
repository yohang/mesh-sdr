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
	"net/url"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/http/api/apitest"
	"github.com/yohang/mesh-sdr/internal/identity"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// API-002 contract tests: the whole hub, its JSON API validated against
// openapi.yaml (apitest), with one account per role. UI actions are HTML
// forms without API twins (ADR 0023): their tests drive the forms.

const contractPassword = "contract test passphrase"

// clientAddr hands out distinct client addresses (X-Forwarded-For from the
// trusted loopback proxy), so the per-address login limit never trips.
var clientAddr atomic.Int32

type contractHub struct {
	t       *testing.T
	url     string
	adapter *db.DB
	users   *identityapp.UserAdmin
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
url = "http://hub.test"
allow_insecure_url = true
[gateway]
tls_mode = "off"
http_listen = "127.0.0.1:0"
storage_dir = "`+filepath.Join(dir, "acme")+`"
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

	admin := identity.UserAdmin(IdentityDeps(cfg, quiet, adapter))
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

	return &contractHub{t: t, url: srv.URL, adapter: adapter, users: admin}
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
	c := &apiClient{h: h, addr: fmt.Sprintf("%d.0.%d.%d", network, n/250, n%250+1), http: &http.Client{
		Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}

	_, body := c.do(http.MethodGet, "/auth/session", nil)
	c.csrf, _ = body["csrf_token"].(string)

	return c
}

// signedIn returns a client signed in as name.
func (h *contractHub) signedIn(name string, network int) *apiClient {
	h.t.Helper()

	c := h.client(network)

	if status := c.form("/login", url.Values{"login": {name}, "password": {contractPassword}}); status != http.StatusSeeOther {
		h.t.Fatalf("login %s = %d", name, status)
	}

	_, body := c.do(http.MethodGet, "/auth/session", nil)
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

// form posts an HTML form to a page path.
func (c *apiClient) form(path string, values url.Values) int {
	c.h.t.Helper()

	return c.post(path, "application/x-www-form-urlencoded", []byte(values.Encode()))
}

// post sends a POST with a body of the given content type to a page path.
func (c *apiClient) post(path, ctype string, body []byte) int {
	c.h.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.h.url+path, bytes.NewReader(body))
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

// avatar returns the multipart body of the Admin › Site upload form, with
// a small PNG for the avatar.
func avatar(t *testing.T) (string, []byte) {
	t.Helper()

	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer

	w := multipart.NewWriter(&body)
	_ = w.WriteField("slot", "avatar")

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
	}
	h := newContractHub(t, v, roles)

	t.Run("rights", func(t *testing.T) { checkRights(t, h, v) })
	t.Run("happy paths", func(t *testing.T) { happyPaths(t, h) })
	t.Run("removed operations", func(t *testing.T) {
		// The UI actions have no API twin any more (ADR 0023): their former
		// paths answer the API's problem+json, even for an admin.
		admin := h.signedIn("admin", 10)

		for _, tc := range []struct {
			method, path, code string
			status             int
		}{
			{http.MethodPost, "/users", "not_found", http.StatusNotFound},
			{http.MethodPost, "/auth/login", "not_found", http.StatusNotFound},
			{http.MethodPatch, "/settings", "not_found", http.StatusNotFound},
			// GET /branding/{slot} stays: the other methods are not allowed.
			{http.MethodPut, "/branding/avatar", "method_not_allowed", http.StatusMethodNotAllowed},
		} {
			if status, res := admin.do(tc.method, tc.path, map[string]any{}); status != tc.status || res["code"] != tc.code {
				t.Errorf("%s %s = %d %v, want %d %s", tc.method, tc.path, status, res, tc.status, tc.code)
			}
		}
	})

	for _, id := range v.Uncovered() {
		t.Errorf("operation %s never answered a validated 2xx", id)
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

			allowed := c.role.Includes(need) && (need != identitydomain.RoleAdmin || c.network == 10)

			switch {
			case allowed && (denial[code] || code == "csrf_failed"):
				t.Errorf("%s %s as %s: refused %d %s", op.Method, op.Path, c.name, status, code)
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

	for _, p := range []string{"/openapi.json", "/healthz/live", "/healthz/ready", "/auth/session", "/features"} {
		expect(anon, http.MethodGet, p, nil, http.StatusOK)
	}

	// The signing keys stay outside /api, at their well-known path.
	res, err := http.Get(h.url + "/.well-known/jwks.json")
	if err != nil {
		t.Fatal(err)
	}

	_ = res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /.well-known/jwks.json = %d", res.StatusCode)
	}

	expect(admin, http.MethodGet, "/config/effective", nil, http.StatusOK)

	// The receiver images are uploaded on Admin › Site.
	ctype, img := avatar(t)
	if status := admin.post("/admin/site/images", ctype, img); status != http.StatusSeeOther {
		t.Errorf("upload the avatar = %d", status)
	}

	if status, _ := anon.do(http.MethodGet, "/branding/avatar", nil); status != http.StatusOK {
		t.Errorf("GET /branding/avatar = %d", status)
	}

	// A node (Admin › Nodes), its capabilities and a device (node reports).
	if status := admin.form("/admin/nodes", url.Values{"id": {"attic"}, "url": {"https://10.8.0.12:8074"}}); status != http.StatusOK {
		t.Fatalf("add a node = %d", status)
	}

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
		ID: shared.MustDeviceID("hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 100_000, FreqMax: 30_000_000,
		SampleRates: []int64{2_048_000},
	}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := gridsqlite.NewDeviceRepository(h.adapter).Save(ctx, dev); err != nil {
		t.Fatal(err)
	}

	features := expect(anon, http.MethodGet, "/features", nil, http.StatusOK)
	if devices, _ := features["devices"].([]any); len(devices) != 1 {
		t.Errorf("features = %v", features)
	}

	// Bookmarks of the device and the band plan (BMK-001, RX-029).
	expect(anon, http.MethodGet, "/bookmarks?device_id=hf&from=100000&to=30000000", nil, http.StatusOK)
	expect(anon, http.MethodGet, "/bandplan?from=100000&to=30000000", nil, http.StatusOK)

	// A file the node of the device sent (FIL-002), and its thumbnail.
	file := storeNodeFile(t, h.adapter, "hf")
	expect(anon, http.MethodGet, "/files/"+file+"/content", nil, http.StatusOK)
	expect(anon, http.MethodGet, "/files/"+file+"/thumbnail", nil, http.StatusOK)

	// POST /auth/token refreshes a media connection the gateway authz
	// issued to the caller (ADR 0012): here an anonymous one, so another
	// caller is refused.
	cid, err := shared.NewUUIDv7(now)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := domain.NewConnection(domain.ConnectionInfo{
		ID: cid, Kind: domain.ConnectionMedia, IP: "10.0.0.1", NodeID: "attic", HubIssued: true,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := gridsqlite.NewConnectionRepository(h.adapter).Open(ctx, conn); err != nil {
		t.Fatal(err)
	}

	expect(anon, http.MethodPost, "/auth/token", map[string]any{"node_id": "attic", "cid": cid.String()}, http.StatusOK)

	if _, res := admin.do(http.MethodPost, "/auth/token", map[string]any{"node_id": "attic", "cid": cid.String()}); res["code"] != "invalid_connection" {
		t.Errorf("token for another caller's connection = %v", res)
	}
}
