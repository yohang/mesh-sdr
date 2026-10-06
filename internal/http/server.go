// Package http wires the HTTP router and server.
package http

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yohang/mesh-sdr/internal/web"
	"github.com/yohang/mesh-sdr/internal/web/templates"
)

// APIPrefix is the base path of the versioned REST API.
const APIPrefix = "/api/v1"

// NewRouter builds the hub router: the web UI and, under APIPrefix, the REST
// API handler (which serves its own problem+json errors).
func NewRouter(logger *slog.Logger, api http.Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(requestLogger(logger))
	r.Use(middleware.Recoverer)

	r.Mount(APIPrefix, api)

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(web.Static())))

	r.Get("/", templ.Handler(templates.Home()).ServeHTTP)

	return r
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

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			next.ServeHTTP(ww, r)

			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}
