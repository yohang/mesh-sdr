// Package http wires the HTTP router and server.
package http

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yohang/mesh-sdr/internal/web"
)

// NewRouter builds the application router.
func NewRouter(logger *slog.Logger, db *sql.DB) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(requestLogger(logger))
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			logger.ErrorContext(r.Context(), "healthcheck failed", slog.Any("error", err))
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(web.Static())))

	// App shell pages and their actions: session + CSRF.
	p := &pages{logger: logger.With(slog.String("component", "http.handler.pages"))}
	sessions := newFakeSessions() // SPIKE: replaced by the DB session store (AUTH-003).

	shell := chi.Chain(
		sessions.middleware,
		csrfProtection(logger.With(slog.String("component", "http.middleware.csrf"))),
	)

	r.NotFound(shell.HandlerFunc(p.notFound).ServeHTTP)

	r.Group(func(r chi.Router) {
		r.Use(shell...)

		r.Get("/", p.receiver)
		r.Get("/map", p.mapPage)
		r.Get("/decodes", p.decodes)
		r.Get("/files", p.files)
		r.Get("/admin", p.admin)
		r.With(requireJSON).Post("/admin/demo", p.adminDemo)
	})

	return r
}

// NewServer builds the HTTP server listening on addr.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
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
