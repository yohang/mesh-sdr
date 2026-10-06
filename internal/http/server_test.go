package http_test

import (
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
