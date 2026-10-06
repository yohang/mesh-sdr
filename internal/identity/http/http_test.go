package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/identity"
	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
	"github.com/yohang/mesh-sdr/internal/mail"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

const (
	hubURL   = "https://hub.example"
	password = "correct horse battery"
)

// pages renders the content alone, or the fragment for htmx requests.
type pages struct{}

func (pages) Page(w http.ResponseWriter, r *http.Request, status int, title string, content, fragment templ.Component) {
	c := content
	if fragment != nil && r.Header.Get("HX-Request") == "true" {
		c = fragment
	} else {
		w.Header().Set("X-Title", title)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = c.Render(r.Context(), w)
}

func (pages) Error(w http.ResponseWriter, _ *http.Request, status int) {
	http.Error(w, http.StatusText(status), status)
}

// adminPage is a test module with an admin-only HTML route.
type adminPage struct{ m *identityhttp.Module }

func (adminPage) Middlewares() []func(http.Handler) http.Handler { return nil }

func (a adminPage) Routes(r chi.Router) {
	r.With(a.m.Require(domain.RoleAdmin)).Get("/admin", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "admin")
	})
	r.Get("/receiver", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "receiver") })
}

type hub struct {
	t       *testing.T
	logs    *syncBuffer
	mail    *outbox
	handler http.Handler
	admin   *app.UserAdmin
	setup   *app.Setup
}

func newHub(t *testing.T, mutate ...func(*config.Hub)) *hub {
	t.Helper()

	ctx := context.Background()
	cfg := config.DefaultHub()
	cfg.Hub.URL = hubURL
	cfg.Auth.Argon2 = config.Argon2{MemoryKiB: 64, Iterations: 1, Parallelism: 1}
	cfg.HTTP.TrustedProxies = []string{"10.0.0.0/8"}

	for _, f := range mutate {
		f(&cfg)
	}

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mails := &outbox{}
	d := identity.Deps{Mail: mails, Config: cfg, Logger: logger, DB: dbtest.NewSQLite(t), IDs: shared.NewUUIDv7Generator(), Now: time.Now}

	m, err := identity.Wire(ctx, d, pages{})
	if err != nil {
		t.Fatal(err)
	}

	srv := api.Server{
		HealthHandlers:     api.NewHealthHandlers(d.DB, logger),
		AuthHandlers:       api.NewAuthHandlers(m.HTTP),
		AccountHandlers:    api.NewAccountHandlers(m.HTTP, m.Accounts, m.Profile),
		InvitationHandlers: api.NewInvitationHandlers(m.HTTP, m.HTTP, m.Invitations),
	}

	return &hub{
		t:       t,
		logs:    logs,
		mail:    mails,
		handler: httpserver.NewRouter(logger, api.NewHandler(srv, m.HTTP, logger), m.HTTP, adminPage{m.HTTP}),
		admin:   identity.UserAdmin(d),
		setup:   m.Setup,
	}
}

// outbox records the queued e-mails.
type outbox struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (o *outbox) Enqueue(m mail.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.sent = append(o.sent, m)

	return nil
}

// last returns the last message sent to an address, and the path of the
// first hub link in it.
func (o *outbox) last(to string) (mail.Message, string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	for i := len(o.sent) - 1; i >= 0; i-- {
		if m := o.sent[i]; m.To == to {
			link := ""
			if _, after, ok := strings.Cut(m.Body, hubURL); ok {
				link, _, _ = strings.Cut(after, "\n")
			}

			return m, link
		}
	}

	return mail.Message{}, ""
}

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

func (h *hub) addUser(name string, role domain.Role) {
	h.t.Helper()

	if _, err := h.admin.Add(context.Background(), app.AddUserInput{Username: name, Role: role, Password: password}); err != nil {
		h.t.Fatal(err)
	}
}

// client is a browser: it keeps cookies (by name) and the CSRF token.
type client struct {
	h       *hub
	cookies map[string]*http.Cookie
	token   string
	remote  string
}

func (h *hub) client() *client {
	return &client{h: h, cookies: map[string]*http.Cookie{}, remote: "192.0.2.1:5555"}
}

func (c *client) do(method, path, ctype, body string, header map[string]string) *http.Response {
	c.h.t.Helper()

	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = c.remote

	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}

	for _, ck := range c.cookies {
		r.AddCookie(ck)
	}

	for k, v := range header {
		r.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	c.h.handler.ServeHTTP(rec, r)
	res := rec.Result()

	for _, ck := range res.Cookies() {
		if ck.MaxAge < 0 {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck
		}
	}

	return res
}

func (c *client) session() map[string]any {
	c.h.t.Helper()

	res := c.do(http.MethodGet, "/api/v1/auth/session", "", "", nil)
	if res.StatusCode != http.StatusOK {
		c.h.t.Fatalf("GET session = %d", res.StatusCode)
	}

	var v map[string]any
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		c.h.t.Fatal(err)
	}

	c.token, _ = v["csrf_token"].(string)

	return v
}

func (c *client) login(login, pw string, remember bool) *http.Response {
	c.h.t.Helper()

	b, _ := json.Marshal(map[string]any{"login": login, "password": pw, "remember_me": remember})

	return c.do(http.MethodPost, "/api/v1/auth/login", "application/json", string(b), map[string]string{identityhttp.CSRFHeader: c.token})
}

func decode(t *testing.T, res *http.Response) map[string]any {
	t.Helper()

	var v map[string]any
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}

	return v
}

func setCookie(res *http.Response, name string) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}

	return nil
}

func TestAnonymousSession(t *testing.T) {
	h := newHub(t)
	c := h.client()

	res := c.do(http.MethodGet, "/api/v1/auth/session", "", "", nil)
	v := decode(t, res)

	if v["authenticated"] != false || v["csrf_token"] == "" || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("session = %v, cache %q", v, res.Header.Get("Cache-Control"))
	}

	pre := setCookie(res, "__Host-rx_presession")
	if pre == nil || !pre.HttpOnly || !pre.Secure || pre.SameSite != http.SameSiteLaxMode || pre.Path != "/" || pre.Domain != "" {
		t.Fatalf("pre-session cookie = %+v", pre)
	}

	token := v["csrf_token"]

	res = c.do(http.MethodGet, "/api/v1/auth/session", "", "", nil)
	if again := decode(t, res); again["csrf_token"] != token || setCookie(res, "__Host-rx_presession") != nil {
		t.Errorf("token changed or cookie reset: %v", again)
	}
}

func TestLoginAndSessionCookie(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleAdmin)

	c := h.client()
	c.session()

	res := c.login("alice", password, false)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", res.StatusCode)
	}

	v := decode(t, res)
	if v["authenticated"] != true || v["csrf_token"] == c.token || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("login response = %v", v)
	}

	sc := setCookie(res, "__Host-rx_session")
	if sc == nil || !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" || sc.Domain != "" || sc.MaxAge != 0 {
		t.Fatalf("session cookie = %+v", sc)
	}

	if pre := setCookie(res, "__Host-rx_presession"); pre == nil || pre.MaxAge >= 0 {
		t.Errorf("pre-session cookie not cleared: %+v", pre)
	}

	s := c.session()
	user, _ := s["user"].(map[string]any)

	if s["authenticated"] != true || user["username"] != "alice" || s["csrf_token"] != v["csrf_token"] {
		t.Errorf("session = %v", s)
	}

	if roles, _ := s["roles"].([]any); len(roles) != 2 || roles[0] != "listener" || roles[1] != "admin" {
		t.Errorf("roles = %v", s["roles"])
	}

	remember := h.client()
	remember.session()

	if sc := setCookie(remember.login("alice", password, true), "__Host-rx_session"); sc == nil || sc.MaxAge != 30*24*3600 {
		t.Errorf("remember-me cookie = %+v", sc)
	}
}

func TestLoginRotatesSession(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleListener)

	c := h.client()
	c.session()
	c.login("alice", password, false)
	first := *c.cookies["__Host-rx_session"]

	c.session()
	c.login("alice", password, false)

	second := c.cookies["__Host-rx_session"]
	if second.Value == first.Value {
		t.Fatal("session id not rotated")
	}

	// The fixed (previous) session is dead.
	old := h.client()
	old.cookies[first.Name] = &first

	if v := old.session(); v["authenticated"] != false {
		t.Error("previous session still valid")
	}
}

func TestLoginErrors(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleListener)

	c := h.client()
	c.session()

	wrong := decode(t, c.login("alice", "wrong password!", false))
	unknown := decode(t, c.login("nobody", password, false))

	if wrong["code"] != "invalid_credentials" || wrong["status"] != float64(401) {
		t.Errorf("wrong password = %v", wrong)
	}

	if wrong["detail"] != unknown["detail"] || wrong["code"] != unknown["code"] {
		t.Errorf("responses differ: %v / %v", wrong, unknown)
	}

	for range 3 {
		c.login("nobody", "x", false)
	}

	res := c.login("nobody", password, false)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" || decode(t, res)["code"] != "rate_limited" {
		t.Errorf("throttled login = %d %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
}

func TestCSRFMatrix(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleListener)

	body := `{"login":"alice","password":"` + password + `"}`

	tests := []struct {
		name   string
		header func(c *client) map[string]string
		ctype  string
		status int
		code   string
	}{
		{"no token", func(*client) map[string]string { return nil }, "application/json", 403, "csrf_failed"},
		{"wrong token", func(*client) map[string]string { return map[string]string{identityhttp.CSRFHeader: "nope"} }, "application/json", 403, "csrf_failed"},
		{"cross-site fetch", func(c *client) map[string]string {
			return map[string]string{identityhttp.CSRFHeader: c.token, "Sec-Fetch-Site": "cross-site"}
		}, "application/json", 403, "csrf_failed"},
		{"foreign origin", func(c *client) map[string]string {
			return map[string]string{identityhttp.CSRFHeader: c.token, "Origin": "https://evil.example"}
		}, "application/json", 403, "csrf_failed"},
		{"text/plain body", func(c *client) map[string]string { return map[string]string{identityhttp.CSRFHeader: c.token} }, "text/plain", 415, "unsupported_media_type"},
		{"form body", func(c *client) map[string]string { return map[string]string{identityhttp.CSRFHeader: c.token} }, "application/x-www-form-urlencoded", 415, "unsupported_media_type"},
		{"trusted origin", func(c *client) map[string]string {
			return map[string]string{identityhttp.CSRFHeader: c.token, "Origin": hubURL, "Sec-Fetch-Site": "same-origin"}
		}, "application/json", 200, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := h.client()
			c.session()

			res := c.do(http.MethodPost, "/api/v1/auth/login", tt.ctype, body, tt.header(c))
			if res.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.status)
			}

			if tt.code != "" {
				if v := decode(t, res); v["code"] != tt.code || res.Header.Get("Content-Type") != "application/problem+json" {
					t.Errorf("problem = %v", v)
				}
			}
		})
	}

	t.Run("token of another visitor", func(t *testing.T) {
		other := h.client()
		other.session()

		c := h.client()
		c.session()

		if res := c.do(http.MethodPost, "/api/v1/auth/login", "application/json", body, map[string]string{identityhttp.CSRFHeader: other.token}); res.StatusCode != 403 {
			t.Errorf("status = %d", res.StatusCode)
		}
	})

	t.Run("pre-session token after login", func(t *testing.T) {
		c := h.client()
		c.session()

		pre := c.token
		c.login("alice", password, false)

		if res := c.do(http.MethodPost, "/api/v1/auth/logout", "", "", map[string]string{identityhttp.CSRFHeader: pre}); res.StatusCode != 403 {
			t.Errorf("logout with the pre-session token = %d", res.StatusCode)
		}
	})

	t.Run("html actions", func(t *testing.T) {
		c := h.client()
		c.session()

		for _, path := range []string{"/login", "/logout"} {
			if res := c.do(http.MethodPost, path, "application/x-www-form-urlencoded", "login=alice&password=x", nil); res.StatusCode != 403 {
				t.Errorf("POST %s without token = %d", path, res.StatusCode)
			}
		}
	})
}

func TestLogout(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleListener)

	c := h.client()
	c.session()

	if res := c.do(http.MethodPost, "/api/v1/auth/logout", "", "", map[string]string{identityhttp.CSRFHeader: c.token}); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous logout = %d", res.StatusCode)
	}

	c.login("alice", password, false)
	c.session()
	session := *c.cookies["__Host-rx_session"]

	res := c.do(http.MethodPost, "/api/v1/auth/logout", "", "", map[string]string{identityhttp.CSRFHeader: c.token})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d", res.StatusCode)
	}

	if ck := setCookie(res, "__Host-rx_session"); ck == nil || ck.MaxAge >= 0 {
		t.Errorf("cookie not cleared: %+v", ck)
	}

	replay := h.client()
	replay.cookies[session.Name] = &session

	res = replay.do(http.MethodGet, "/api/v1/auth/session", "", "", nil)
	if v := decode(t, res); v["authenticated"] != false {
		t.Error("session valid after logout")
	}

	if ck := setCookie(res, "__Host-rx_session"); ck == nil || ck.MaxAge >= 0 {
		t.Error("stale session cookie not cleared")
	}
}

func TestLoginPage(t *testing.T) {
	h := newHub(t)
	h.addUser("alice", domain.RoleListener)

	c := h.client()

	res := c.do(http.MethodGet, "/login?next="+url.QueryEscape("//evil.example/x"), "", "", nil)
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Robots-Tag") != "noindex" {
		t.Errorf("GET /login = %d %v", res.StatusCode, res.Header)
	}

	for _, want := range []string{`name="login"`, `autocomplete="username"`, `autocomplete="current-password"`, `name="next" value="/"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("login page misses %s", want)
		}
	}

	res = c.do(http.MethodGet, "/login?next="+url.QueryEscape("/receiver?f=1"), "", "", nil)
	if body, _ := io.ReadAll(res.Body); !strings.Contains(string(body), `value="/receiver?f=1"`) {
		t.Error("safe next dropped")
	}

	c.session()

	htmx := map[string]string{identityhttp.CSRFHeader: c.token, "HX-Request": "true"}

	res = c.do(http.MethodPost, "/login", "application/x-www-form-urlencoded", "login=alice&password=wrong&next=/receiver", htmx)
	body, _ = io.ReadAll(res.Body)

	if res.StatusCode != 401 || !strings.Contains(string(body), "Incorrect username, e-mail or password.") || strings.Contains(string(body), "<h1") {
		t.Errorf("failed login fragment = %d %s", res.StatusCode, body)
	}

	res = c.do(http.MethodPost, "/login", "application/x-www-form-urlencoded",
		"login=alice&password="+url.QueryEscape(password)+"&next=/receiver", htmx)
	if res.StatusCode != http.StatusNoContent || res.Header.Get("HX-Redirect") != "/receiver" || setCookie(res, "__Host-rx_session") == nil {
		t.Errorf("login = %d %q", res.StatusCode, res.Header.Get("HX-Redirect"))
	}

	c.session()

	res = c.do(http.MethodPost, "/logout", "", "", map[string]string{identityhttp.CSRFHeader: c.token})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Errorf("logout = %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	if v := c.session(); v["authenticated"] != false {
		t.Error("still signed in after logout")
	}
}

func TestAuthBodiesAreBounded(t *testing.T) {
	h := newHub(t)
	c := h.client()
	c.session()

	big := strings.Repeat("a", identityhttp.AuthBodyLimit)
	hdr := map[string]string{identityhttp.CSRFHeader: c.token}

	if res := c.do(http.MethodPost, "/login", "application/x-www-form-urlencoded", "login=alice&password="+big, hdr); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("large form = %d", res.StatusCode)
	}

	res := c.do(http.MethodPost, "/api/v1/auth/login", "application/json", `{"login":"alice","password":"`+big+`"}`, hdr)
	if res.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(decode(t, res)["detail"].(string), "too large") {
		t.Errorf("large JSON body = %d", res.StatusCode)
	}
}

func TestSafeNext(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/", func(http.ResponseWriter, *http.Request) {})
	r.Get("/receiver", func(http.ResponseWriter, *http.Request) {})
	r.Get("/files/{id}", func(http.ResponseWriter, *http.Request) {})
	r.Get("/setup/{token}", func(http.ResponseWriter, *http.Request) {})

	tests := map[string]string{
		"":                       "/",
		"/receiver":              "/receiver",
		"/receiver?f=7100000#x":  "/receiver?f=7100000",
		"/files/42":              "/files/42",
		"/unknown":               "/",
		"//evil.example":         "/",
		"/\\evil.example":        "/",
		"https://evil.example/":  "/",
		"javascript:alert(1)":    "/",
		"receiver":               "/",
		"/%2F%2Fevil.example":    "/",
		"/receiver\r\nLocation:": "/",
		"/%5Cevil":               "/",
		"/ok?next=//evil":        "/",
		// A single-use token never goes into a redirect.
		"/setup/s3cr3t":     "/",
		"/setup/s3cr3t?x=1": "/",
		"/%73etup/s3cr3t":   "/",
	}

	for in, want := range tests {
		if got := identityhttp.SafeNext(in, r); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAdminRequiresRoleAndNetwork(t *testing.T) {
	h := newHub(t, func(c *config.Hub) { c.Admin.AllowedNetworks = []string{"192.0.2.0/24"} })
	h.addUser("alice", domain.RoleListener)
	h.addUser("root", domain.RoleAdmin)

	anon := h.client()
	if res := anon.do(http.MethodGet, "/admin", "", "", nil); res.StatusCode != http.StatusSeeOther ||
		res.Header.Get("Location") != "/login?next=%2Fadmin" {
		t.Errorf("anonymous = %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	listener := h.client()
	listener.session()
	listener.login("alice", password, false)

	if res := listener.do(http.MethodGet, "/admin", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener = %d", res.StatusCode)
	}

	admin := h.client()
	admin.session()
	admin.login("root", password, false)

	if res := admin.do(http.MethodGet, "/admin", "", "", nil); res.StatusCode != http.StatusOK {
		t.Errorf("admin in network = %d", res.StatusCode)
	}

	// Same session from another network: refused on admin routes only.
	admin.remote = "198.51.100.9:1"

	if res := admin.do(http.MethodGet, "/admin", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("admin outside the network = %d", res.StatusCode)
	}

	if res := admin.do(http.MethodGet, "/receiver", "", "", nil); res.StatusCode != http.StatusOK {
		t.Errorf("non-admin route from outside = %d", res.StatusCode)
	}

	// A forged X-Forwarded-For from an untrusted peer does not help…
	if res := admin.do(http.MethodGet, "/admin", "", "", map[string]string{"X-Forwarded-For": "192.0.2.50"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("forged XFF = %d", res.StatusCode)
	}

	// …but the same header from a trusted proxy does.
	admin.remote = "10.1.2.3:80"

	if res := admin.do(http.MethodGet, "/admin", "", "", map[string]string{"X-Forwarded-For": "192.0.2.50"}); res.StatusCode != http.StatusOK {
		t.Errorf("XFF from a trusted proxy = %d", res.StatusCode)
	}
}

func TestAuthorize(t *testing.T) {
	m, err := identityhttp.New(identityhttp.Services{}, pages{}, identityhttp.Config{
		HubURL: hubURL, AdminNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	if err := m.Authorize(ctx, domain.RoleAnonymous); err != nil {
		t.Errorf("anonymous operation: %v", err)
	}

	if err := m.Authorize(ctx, domain.RoleListener); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("listener operation, anonymous caller: %v", err)
	}

	if _, err := identityhttp.New(identityhttp.Services{}, pages{}, identityhttp.Config{HubURL: "not a url"}, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("invalid hub.url accepted")
	}

	var logs bytes.Buffer

	if _, err := identityhttp.New(identityhttp.Services{}, pages{}, identityhttp.Config{HubURL: "http://lan.example"}, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "without Secure") {
		t.Errorf("no warning for an http hub.url: %q", logs.String())
	}
}
