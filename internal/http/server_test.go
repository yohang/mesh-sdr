package http_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestNewServerTimeouts(t *testing.T) {
	srv := httpserver.NewServer(":0", http.NotFoundHandler())

	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.IdleTimeout == 0 {
		t.Errorf("timeouts = header %v, read %v, idle %v; want all set", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}

	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want unset (long-lived WebSocket responses)", srv.WriteTimeout)
	}
}
