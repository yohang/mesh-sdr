// Package http wires the HTTP router and server.
package http

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/http/redact"
	"github.com/yohang/mesh-sdr/internal/web"
)

// APIPrefix is the base path of the versioned REST API.
const APIPrefix = "/api/v1"

// Module is a part of the hub that contributes to the router: global
// middlewares and routes.
type Module interface {
	// Middlewares wrap every route (API, static assets and pages). They run
	// in module order, after the router's own middlewares (request id,
	// logging, panic recovery, security headers).
	Middlewares() []func(http.Handler) http.Handler
	// Routes registers the module's routes. It runs after every module's
	// middlewares are installed, so it must not call r.Use (use r.Group or
	// r.With for scoped middlewares).
	Routes(r chi.Router)
}

// NewRouter builds the hub router: the router middlewares, then each
// module's middlewares, then the REST API handler under APIPrefix (which
// serves its own problem+json errors, like any other path under /api), the
// static assets and each module's routes.
func NewRouter(logger *slog.Logger, api http.Handler, modules ...Module) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(requestLogger(logger))
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	for _, m := range modules {
		r.Use(m.Middlewares()...)
	}

	r.Mount(APIPrefix, api)
	// Every other path under /api (an unversioned or unknown version, a
	// typo) answers the API's problem+json 404, never the HTML shell page
	// (API-002: one error format).
	r.Handle("/api", http.HandlerFunc(problem.NotFound))
	r.Handle("/api/*", http.HandlerFunc(problem.NotFound))

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(web.Static())))

	for _, m := range modules {
		m.Routes(r)
	}

	return r
}

// LimitBody caps request bodies at n bytes (gateway.max_body, §4.6).
func LimitBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Server timeouts. WriteTimeout stays unset: it would cut long-lived
// WebSocket and streaming responses. Hijacked (WebSocket) connections must
// clear the read deadline set by ReadTimeout.
const (
	ReadHeaderTimeout = 10 * time.Second
	ReadTimeout       = 30 * time.Second
	IdleTimeout       = 120 * time.Second
)

// NewServer builds the HTTP server listening on addr (hub and node).
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		IdleTimeout:       IdleTimeout,
	}
}

// loggedPath is the request path as logged: a single-use token in it is
// redacted (SR-07), whether or not a route matched.
func loggedPath(r *http.Request) string { return redact.Path(r.URL.Path) }

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			next.ServeHTTP(ww, r)

			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", loggedPath(r)),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}
