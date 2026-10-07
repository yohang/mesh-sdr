package http_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	httpserver "github.com/yohang/mesh-sdr/internal/http"
)

// module records its middleware in the X-Trace response header and serves
// GET /<name>.
type module struct{ name string }

func (m module) Middlewares() []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("X-Trace", m.name)
				next.ServeHTTP(w, r)
			})
		},
	}
}

func (m module) Routes(r chi.Router) {
	r.Get("/"+m.name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(m.name)) })
}

func TestNewRouterModules(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("api")) })
	h := httpserver.NewRouter(slog.New(slog.DiscardHandler), api, module{"a"}, module{"b"})

	tests := []struct{ path, body string }{
		{"/a", "a"},
		{"/b", "b"},
		{httpserver.APIPrefix + "/x", "api"},
	}

	for _, tt := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

		if rec.Code != http.StatusOK || rec.Body.String() != tt.body {
			t.Errorf("GET %s = %d %q, want %q", tt.path, rec.Code, rec.Body.String(), tt.body)
		}

		// Module middlewares wrap every route, API included, in module order.
		if got := strings.Join(rec.Header().Values("X-Trace"), ","); got != "a,b" {
			t.Errorf("GET %s: middleware order = %q, want a,b", tt.path, got)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	var nonces []string

	page := func(w http.ResponseWriter, r *http.Request) {
		nonce := templ.GetNonce(r.Context())
		nonces = append(nonces, nonce)
		_, _ = w.Write([]byte(nonce))
	}

	h := httpserver.NewRouter(slog.New(slog.DiscardHandler), http.HandlerFunc(page), routes(func(r chi.Router) { r.Get("/page", page) }))

	for _, path := range []string{"/page", "/page", httpserver.APIPrefix + "/x"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		hdr := rec.Header()
		want := map[string]string{
			"X-Content-Type-Options":     "nosniff",
			"Referrer-Policy":            "same-origin",
			"Cross-Origin-Opener-Policy": "same-origin",
			"Permissions-Policy":         httpserver.PermissionsPolicy,
		}

		for k, v := range want {
			if got := hdr.Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", path, k, got, v)
			}
		}

		nonce := rec.Body.String()
		if len(nonce) < 22 {
			t.Fatalf("%s: nonce %q too short", path, nonce)
		}

		if got := hdr.Get("Content-Security-Policy"); got != httpserver.ContentSecurityPolicy(nonce) {
			t.Errorf("%s: CSP = %q", path, got)
		}
	}

	if nonces[0] == nonces[1] || nonces[1] == nonces[2] {
		t.Errorf("nonce reused across requests: %v", nonces)
	}

	csp := httpserver.ContentSecurityPolicy("n")
	for _, d := range []string{"script-src 'nonce-n';", "frame-ancestors 'none'", "style-src 'self';", "default-src 'none'"} {
		if !strings.Contains(csp, d) {
			t.Errorf("CSP lacks %q: %s", d, csp)
		}
	}

	for _, banned := range []string{"unsafe-inline", "unsafe-eval", "script-src 'self'"} {
		if strings.Contains(csp, banned) {
			t.Errorf("CSP contains %q: %s", banned, csp)
		}
	}
}

func TestPermissionsPolicy(t *testing.T) {
	directives := map[string]bool{}

	for d := range strings.SplitSeq(httpserver.PermissionsPolicy, ", ") {
		feature, allow, ok := strings.Cut(d, "=")
		if !ok || allow != "()" {
			t.Errorf("directive %q does not deny the feature", d)
		}

		directives[feature] = true
	}

	for _, f := range []string{"camera", "microphone", "geolocation", "usb", "serial", "hid", "payment", "display-capture", "midi"} {
		if !directives[f] {
			t.Errorf("Permissions-Policy does not deny %s", f)
		}
	}
}

// routes is a Module with routes only.
type routes func(r chi.Router)

func (routes) Middlewares() []func(http.Handler) http.Handler { return nil }
func (f routes) Routes(r chi.Router)                          { f(r) }

func TestNewServerTimeouts(t *testing.T) {
	srv := httpserver.NewServer(":0", http.NotFoundHandler())

	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.IdleTimeout == 0 {
		t.Errorf("timeouts = header %v, read %v, idle %v; want all set", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}

	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want unset (long-lived WebSocket responses)", srv.WriteTimeout)
	}
}

// tokenModule serves a route that carries a single-use token.
type tokenModule struct{}

func (tokenModule) Middlewares() []func(http.Handler) http.Handler { return nil }

func (tokenModule) Routes(r chi.Router) {
	r.Get("/invite/{token}", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
}

func TestRequestLogHidesTokens(t *testing.T) {
	var logs strings.Builder

	h := httpserver.NewRouter(slog.New(slog.NewTextHandler(&logs, nil)), http.NotFoundHandler(), tokenModule{}, module{"a"})

	for _, path := range []string{"/invite/s3cr3t-t0ken", "/a"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	if out := logs.String(); strings.Contains(out, "s3cr3t-t0ken") || !strings.Contains(out, "path=/invite/{token}") ||
		!strings.Contains(out, "path=/a ") {
		t.Errorf("logs = %s", out)
	}
}

// TestAPIPathsAnswerProblems covers API-002's single error format: any path
// under /api outside the versioned API answers a problem+json 404, whatever
// the method, never the HTML 404 of a module.
func TestAPIPathsAnswerProblems(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("api")) })
	html := routes(func(r chi.Router) {
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "html 404", http.StatusNotFound) })
	})
	h := httpserver.NewRouter(slog.New(slog.DiscardHandler), api, html)

	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/api"},
		{http.MethodGet, "/api/"},
		{http.MethodGet, "/api/v2/nodes"},
		{http.MethodPost, "/api/v0"},
		{http.MethodDelete, "/api/nope/deeper"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

		if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/problem+json" ||
			!strings.Contains(rec.Body.String(), `"code":"not_found"`) {
			t.Errorf("%s %s = %d %q %s", tt.method, tt.path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/apis", nil))

	if rec.Body.String() != "html 404\n" {
		t.Errorf("GET /apis = %q, want the module's 404", rec.Body.String())
	}
}

func TestLimitBody(t *testing.T) {
	h := httpserver.LimitBody(10)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)

				return
			}
		}

		w.WriteHeader(http.StatusNoContent)
	}))

	for _, tc := range []struct {
		body string
		want int
	}{{"small", http.StatusNoContent}, {"far too large for the limit", http.StatusRequestEntityTooLarge}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(tc.body)))

		if rec.Code != tc.want {
			t.Errorf("%q: status %d, want %d", tc.body, rec.Code, tc.want)
		}
	}
}
