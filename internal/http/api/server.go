// Package api is the hub JSON API under /api/v1: only what scripts,
// islands, nodes and operations need (ADR 0023). openapi.yaml is the source
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
	AuthHandlers
	ConfigHandlers
	BrandingHandlers
	TokenHandlers
	FeatureHandlers
	StatusHandlers
	BookmarkHandlers
	FileHandlers
}

// BookmarkHandlers serve GET /bookmarks and GET /bandplan; the bookmarks
// module implements them (internal/bookmarks).
type BookmarkHandlers interface {
	GetBookmarks(ctx context.Context, req GetBookmarksRequestObject) (GetBookmarksResponseObject, error)
	GetBandplan(ctx context.Context, req GetBandplanRequestObject) (GetBandplanResponseObject, error)
}

var _ StrictServerInterface = Server{}

// NewHandler returns the /api/v1 handler: generated routes, the access
// policy of openapi.yaml (x-meshsdr-access) checked by authz, request bodies
// capped at maxBody, problem+json errors (including 404, 405 and panics).
// It fails when an operation of the embedded document has no access level.
func NewHandler(srv StrictServerInterface, authz Authorizer, maxBody int64, logger *slog.Logger) (http.Handler, error) {
	return newHandler(specJSON, srv, authz, maxBody, logger)
}

func newHandler(doc []byte, srv StrictServerInterface, authz Authorizer, maxBody int64, logger *slog.Logger) (http.Handler, error) {
	s, err := parseSpec(doc)
	if err != nil {
		return nil, err
	}

	r := chi.NewRouter()
	r.Use(problem.Recoverer(logger), s.guard(authz, maxBody))
	r.NotFound(problem.NotFound)
	r.MethodNotAllowed(problem.MethodNotAllowed)

	strict := NewStrictHandlerWithOptions(srv, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  problem.BadRequest,
		ResponseErrorHandlerFunc: problem.ErrorHandler(logger),
	})

	return HandlerWithOptions(strict, ChiServerOptions{
		BaseRouter: r, ErrorHandlerFunc: problem.BadRequest,
		Middlewares: []MiddlewareFunc{s.bodies.rejectUnknownFields},
	}), nil
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

// Pinger checks a backing service (the database).
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
	return GetLiveness200JSONResponse{Status: HealthStatusOk}, nil
}

// GetReadiness implements StrictServerInterface.
func (h HealthHandlers) GetReadiness(ctx context.Context, _ GetReadinessRequestObject) (GetReadinessResponseObject, error) {
	if err := h.db.Ping(ctx); err != nil {
		h.logger.WarnContext(ctx, "not ready: database unavailable", slog.Any("error", err))

		return GetReadiness503JSONResponse{Status: HealthStatusDegraded}, nil
	}

	return GetReadiness200JSONResponse{Status: HealthStatusOk}, nil
}
