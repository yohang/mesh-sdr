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
	AuthHandlers
	GridHandlers
	SettingsHandlers
	RetentionHandlers
	BrandingHandlers
	AccountHandlers
	InvitationHandlers
	ResetHandlers
	AuditHandlers
	TokenHandlers
	FeatureHandlers
	PresetHandlers
	ScheduleHandlers
}

var _ StrictServerInterface = Server{}

// NewHandler returns the /api/v1 handler: generated routes, the access
// policy of openapi.yaml (x-meshsdr-access) checked by authz, problem+json
// errors (including 404, 405 and panics). It panics when an operation of
// the embedded document has no access level (a build defect, caught by
// tests).
func NewHandler(srv StrictServerInterface, authz Authorizer, logger *slog.Logger) http.Handler {
	policy, err := LoadPolicy(specJSON)
	if err != nil {
		panic(err)
	}

	fields, err := loadBodyFields(specJSON)
	if err != nil {
		panic(err)
	}

	r := chi.NewRouter()
	r.Use(problem.Recoverer(logger), guard(authz), uploadLimits)
	r.NotFound(problem.NotFound)
	r.MethodNotAllowed(problem.MethodNotAllowed)

	strict := NewStrictHandlerWithOptions(srv, []StrictMiddlewareFunc{policy.Middleware(authz)}, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  problem.BadRequest,
		ResponseErrorHandlerFunc: problem.ErrorHandler(logger),
	})

	return HandlerWithOptions(strict, ChiServerOptions{
		BaseRouter: r, ErrorHandlerFunc: problem.BadRequest,
		Middlewares: []MiddlewareFunc{fields.rejectUnknownFields(policy, authz)},
	})
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
