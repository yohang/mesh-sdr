package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// AccessExtension is the OpenAPI operation extension that declares the role
// an operation requires: the server-side policy table (SR-15, SR-18).
const AccessExtension = "x-meshsdr-access"

// Authorizer checks that the caller of a request holds a role (and, for
// admin operations, comes from an allowed network).
type Authorizer interface {
	Authorize(ctx context.Context, role domain.Role) error
}

// Policy maps each operation (by its generated Go name, e.g. "GetSession")
// to the role it requires.
type Policy map[string]domain.Role

// LoadPolicy reads the access level of every operation of an OpenAPI
// document. An operation without a valid access level is an error: the
// policy fails closed.
func LoadPolicy(spec []byte) (Policy, error) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}

	if err := json.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("parse OpenAPI document: %w", err)
	}

	p := Policy{}

	for path, item := range doc.Paths {
		for method, raw := range item {
			switch strings.ToUpper(method) {
			case http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete,
				http.MethodOptions, http.MethodHead, http.MethodPatch, http.MethodTrace:
			default:
				continue // parameters, summary, …
			}

			var op struct {
				ID     string `json:"operationId"`
				Access string `json:"x-meshsdr-access"`
			}

			if err := json.Unmarshal(raw, &op); err != nil {
				return nil, fmt.Errorf("%s %s: %w", method, path, err)
			}

			role, err := domain.ParseRole(op.Access)
			if err != nil || op.ID == "" {
				return nil, fmt.Errorf("%s %s: missing operationId or invalid %s %q", strings.ToUpper(method), path, AccessExtension, op.Access)
			}

			p[goName(op.ID)] = role
		}
	}

	return p, nil
}

// goName is the operation name oapi-codegen passes to strict middlewares.
func goName(id string) string {
	r, n := utf8.DecodeRuneInString(id)

	return string(unicode.ToUpper(r)) + id[n:]
}

// Middleware returns the strict middleware enforcing the policy: an
// operation missing from the policy is refused.
func (p Policy) Middleware(authz Authorizer) StrictMiddlewareFunc {
	return func(f StrictHandlerFunc, operationID string) StrictHandlerFunc {
		role, ok := p[operationID]

		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			if !ok {
				return nil, domain.ErrForbidden.WithDetail("operation without an access level")
			}

			if err := authz.Authorize(ctx, role); err != nil {
				return nil, err
			}

			return f(ctx, w, r, request)
		}
	}
}
