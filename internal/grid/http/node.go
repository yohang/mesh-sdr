// Package http holds the grid HTTP handlers.
package http

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/http/problem"
)

// NewPreEnrollmentRouter returns the node API of a node that is not enrolled
// yet (TECHNICAL_SPEC §4.2 step 3): only POST /enroll exists and every other
// path answers 403. Enrollment is served by the one-off `meshsdr node
// enroll` (ADR 0008 Q13), so /enroll answers 501 here.
func NewPreEnrollmentRouter(logger *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(problem.Recoverer(logger))

	forbidden := func(w http.ResponseWriter, r *http.Request) {
		logger.DebugContext(r.Context(), "request refused: node not enrolled",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("remote_addr", r.RemoteAddr))
		problem.Write(w, problem.New(http.StatusForbidden, problem.CodeForbidden, "node is not enrolled"))
	}

	r.NotFound(forbidden)
	r.MethodNotAllowed(forbidden)

	r.Post("/enroll", func(w http.ResponseWriter, r *http.Request) {
		logger.WarnContext(r.Context(), "enrollment attempt refused: run `meshsdr node enroll` on this node", slog.String("remote_addr", r.RemoteAddr))
		problem.Write(w, problem.New(http.StatusNotImplemented, problem.CodeNotImplemented, "this node is not waiting for enrollment: run `meshsdr node enroll`"))
	})

	return r
}
