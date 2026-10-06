package wire_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/wire"
)

var discard = slog.New(slog.DiscardHandler)

// serve runs p on a random local port and returns its base address.
func serve(t *testing.T, p *wire.Process) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	ln, err := p.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- p.Serve(ctx, ln) }()

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("graceful shutdown timed out")
		}
	})

	return ln.Addr().String()
}

func hub(t *testing.T, cfg config.Hub, a db.Adapter) *wire.Process {
	t.Helper()

	cfg.Hub.URL = "http://" + cfg.Hub.Listen
	cfg.Hub.AllowInsecureURL = true
	cfg.Auth.Argon2 = config.Argon2{MemoryKiB: 64, Iterations: 1, Parallelism: 1}
	cfg.Auth.TokenKeyDir = filepath.Join(t.TempDir(), "keys")

	p, err := wire.Hub(context.Background(), cfg, config.Origins{}, discard, a)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func get(t *testing.T, c *http.Client, url string) (int, string, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

func TestHub(t *testing.T) {
	cfg := config.DefaultHub()
	cfg.Hub.Listen = "127.0.0.1:0"

	addr := serve(t, hub(t, cfg, dbtest.NewSQLite(t)))
	base := "http://" + addr

	tests := []struct {
		path   string
		status int
		ctype  string
	}{
		{"/api/v1/healthz/live", 200, "application/json"},
		{"/api/v1/healthz/ready", 200, "application/json"},
		{"/api/v1/openapi.json", 200, "application/json"},
		{"/api/v1/nope", 404, "application/problem+json"},
		{"/", 200, "text/html; charset=utf-8"},
		{"/nope", 404, "text/html; charset=utf-8"},
		{"/login", 200, "text/html; charset=utf-8"},
		{"/api/v1/auth/session", 200, "application/json"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			status, ctype, body := get(t, http.DefaultClient, base+tt.path)
			if status != tt.status || ctype != tt.ctype {
				t.Fatalf("got %d %q, want %d %q: %s", status, ctype, tt.status, tt.ctype, body)
			}

			if !json.Valid(body) && tt.ctype != "text/html; charset=utf-8" {
				t.Fatalf("invalid JSON: %s", body)
			}
		})
	}

	_, _, spec := get(t, http.DefaultClient, base+"/api/v1/openapi.json")

	var doc struct {
		OpenAPI string         `json:"openapi"`
		Paths   map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil || doc.OpenAPI == "" || doc.Paths["/healthz/ready"] == nil {
		t.Fatalf("openapi.json = %s (%v)", spec, err)
	}
}

type downDB struct{ db.Adapter }

func (downDB) Ping(context.Context) error { return errors.New("down") }

func TestHubNotReady(t *testing.T) {
	cfg := config.DefaultHub()
	cfg.Hub.Listen = "127.0.0.1:0"

	addr := serve(t, hub(t, cfg, downDB{dbtest.NewSQLite(t)}))

	status, _, body := get(t, http.DefaultClient, "http://"+addr+"/api/v1/healthz/ready")
	if status != http.StatusServiceUnavailable || !json.Valid(body) {
		t.Fatalf("got %d %s", status, body)
	}
}

func TestNode(t *testing.T) {
	cfg := config.DefaultNode()
	cfg.Node.ID = "attic"
	cfg.Node.Listen = "127.0.0.1:0"

	p, err := wire.Node(cfg, discard, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	addr := serve(t, p)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed pre-enrollment certificate
	}}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://"+addr+"/enroll", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST /enroll = %d", resp.StatusCode)
	}

	if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("TLS = %+v, want TLS 1.3", resp.TLS)
	}

	if uris := resp.TLS.PeerCertificates[0].URIs; len(uris) != 1 || uris[0].String() != "urn:rx:node:attic" {
		t.Errorf("peer URIs = %v", uris)
	}

	if status, _, _ := get(t, client, "https://"+addr+"/control"); status != http.StatusForbidden {
		t.Errorf("GET /control = %d, want 403", status)
	}
}

func TestOpenDB(t *testing.T) {
	ctx := context.Background()

	a, err := wire.OpenDB(ctx, config.DB{DSN: "sqlite://" + filepath.Join(t.TempDir(), "hub.db"), MaxReadConnections: 2}, discard)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = a.Close() })

	if a.Dialect() != db.DialectSQLite {
		t.Errorf("dialect = %s", a.Dialect())
	}

	if _, err := wire.OpenDB(ctx, config.DB{DSN: "postgres://x/y"}, discard); !errors.Is(err, db.ErrEngineUnsupported) {
		t.Errorf("postgres: err = %v", err)
	}
}

// The login page renders in the app shell, with the shell's security
// headers, and coexists with the shell's HEAD and 405 handling.
func TestLoginPageInShell(t *testing.T) {
	cfg := config.DefaultHub()
	cfg.Hub.Listen = "127.0.0.1:0"
	base := "http://" + serve(t, hub(t, cfg, dbtest.NewSQLite(t)))

	do := func(method, path string) (*http.Response, string) {
		t.Helper()

		req, err := http.NewRequestWithContext(context.Background(), method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = resp.Body.Close() }()

		b, _ := io.ReadAll(resp.Body)

		return resp, string(b)
	}

	resp, body := do(http.MethodGet, "/login")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login = %d", resp.StatusCode)
	}

	for _, h := range []string{"Content-Security-Policy", "Permissions-Policy", "Referrer-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("GET /login without %s", h)
		}
	}

	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "'nonce-") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", resp.Header)
	}

	for _, want := range []string{`id="main"`, `/static/js/shell.js`, `<title>Sign in`, `id="login-form"`} {
		if !strings.Contains(body, want) {
			t.Errorf("login page misses %s", want)
		}
	}

	if resp, body := do(http.MethodHead, "/login"); resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD /login = %d %q", resp.StatusCode, body)
	}

	// Safe methods get the shell's 405 with Allow; unsafe ones meet the
	// CSRF check before routing.
	if resp, _ := do(http.MethodOptions, "/login"); resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD, POST" {
		t.Errorf("OPTIONS /login = %d, Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}

	if resp, _ := do(http.MethodPut, "/login"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("PUT /login without a CSRF token = %d", resp.StatusCode)
	}

	if resp, _ := do(http.MethodGet, "/logout"); resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Errorf("GET /logout = %d, Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

// ACC-001: the hub works without any account. Visitors browse the public
// pages, see a discreet "Sign in" entry, and protected pages ask them to
// sign in.
func TestHubWithoutAccounts(t *testing.T) {
	cfg := config.DefaultHub()
	cfg.Hub.Listen = "127.0.0.1:0"

	addr := serve(t, hub(t, cfg, dbtest.NewSQLite(t)))
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	for _, path := range []string{"/", "/policy", "/login", "/password/forgot"} {
		status, _, b := get(t, c, "http://"+addr+path)
		if status != http.StatusOK || !strings.Contains(string(b), `href="/login"`) && path != "/login" {
			t.Errorf("GET %s = %d", path, status)
		}
	}

	_, _, b := get(t, c, "http://"+addr+"/api/v1/auth/session")

	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil || s["authenticated"] != false {
		t.Errorf("session = %s", b)
	}

	for _, path := range []string{"/account", "/admin/users"} {
		if status, _, _ := get(t, c, "http://"+addr+path); status != http.StatusSeeOther {
			t.Errorf("GET %s = %d, want a redirect to sign in", path, status)
		}
	}
}

// SR-64: removing a user from the CLI also erases its presence rows.
func TestRemoveErasesConnections(t *testing.T) {
	ctx := context.Background()
	cfg := config.DefaultHub()
	cfg.Auth.Argon2 = config.Argon2{MemoryKiB: 64, Iterations: 1, Parallelism: 1}
	a := dbtest.NewSQLite(t)
	admin := wire.UserAdmin(cfg, discard, a)

	res, err := admin.Add(ctx, identityapp.AddUserInput{Username: "alice", Password: "a long passphrase here"})
	if err != nil {
		t.Fatal(err)
	}

	uid, _ := shared.UUIDFromBytes(res.User.ID().Bytes())
	cid, _ := shared.NewUUIDv7Generator().New(time.Now())

	c, err := griddomain.NewConnection(griddomain.ConnectionInfo{ID: cid, Kind: griddomain.ConnectionEvents, UserID: uid, RoleID: 10, IP: "192.0.2.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	conns := gridsqlite.NewConnectionRepository(a)
	if _, err := conns.Open(ctx, c); err != nil {
		t.Fatal(err)
	}

	if err := admin.Remove(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	got, err := conns.Get(ctx, cid)
	if err != nil || !got.Info().UserID.IsZero() || got.Info().IP != "" {
		t.Errorf("connection after removal = %+v, %v", got, err)
	}
}
