// Package api is the hub REST API under /api/v1. openapi.yaml is the source
// of truth: oapi-codegen generates the chi strict server (api.gen.go) and the
// document is embedded as openapi.json.
//
// Server implements the generated StrictServerInterface by embedding one
// handler struct per module; each module adds its own handlers to it.
package api

import (
	"context"
	_ "embed"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/http/problem"
)

//go:embed openapi.json
var specJSON []byte

// Spec returns the OpenAPI document as JSON.
func Spec() []byte { return specJSON }

// Server implements StrictServerInterface by composing module handlers.
type Server struct {
	MetaHandlers
	HealthHandlers
}

var _ StrictServerInterface = Server{}

// NewHandler returns the /api/v1 handler: generated routes, problem+json
// errors (including 404, 405 and panics).
func NewHandler(srv StrictServerInterface, logger *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(problem.Recoverer(logger))
	r.NotFound(problem.NotFound)
	r.MethodNotAllowed(problem.MethodNotAllowed)

	strict := NewStrictHandlerWithOptions(srv, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  problem.BadRequest,
		ResponseErrorHandlerFunc: problem.ErrorHandler(logger),
	})

	return HandlerWithOptions(strict, ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: problem.BadRequest})
}

// MetaHandlers serve the API description.
type MetaHandlers struct{}

// GetOpenAPI implements StrictServerInterface.
func (MetaHandlers) GetOpenAPI(context.Context, GetOpenAPIRequestObject) (GetOpenAPIResponseObject, error) {
	return rawJSON(specJSON), nil
}

// rawJSON writes pre-encoded JSON as is.
type rawJSON []byte

func (b rawJSON) VisitGetOpenAPIResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(b)

	return err
}

// Pinger checks a backing service (the database adapter).
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthHandlers serve the liveness and readiness probes.
type HealthHandlers struct {
	db     Pinger
	logger *slog.Logger
}

// NewHealthHandlers returns the health handlers. db is checked by readiness.
func NewHealthHandlers(db Pinger, logger *slog.Logger) HealthHandlers {
	return HealthHandlers{db: db, logger: logger}
}

// GetLiveness implements StrictServerInterface.
func (HealthHandlers) GetLiveness(context.Context, GetLivenessRequestObject) (GetLivenessResponseObject, error) {
	return GetLiveness200JSONResponse{Status: Ok}, nil
}

// GetReadiness implements StrictServerInterface.
func (h HealthHandlers) GetReadiness(ctx context.Context, _ GetReadinessRequestObject) (GetReadinessResponseObject, error) {
	if err := h.db.Ping(ctx); err != nil {
		h.logger.WarnContext(ctx, "not ready: database unavailable", slog.Any("error", err))

		return GetReadiness503JSONResponse{Status: Degraded}, nil
	}

	return GetReadiness200JSONResponse{Status: Ok}, nil
}
