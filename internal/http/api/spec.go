package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
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

// spec is what the handler reads of the OpenAPI document: the operations
// with their access level, the most specific paths (fewest parameters)
// first, and the closed request bodies.
type spec struct {
	operations []operation
	bodies     bodyFields
}

// operation is an operation of openapi.yaml matched by method and path.
type operation struct {
	// name is the generated Go name of the operation (e.g. "GetSession").
	name   string
	method string
	path   *regexp.Regexp
	params int
	role   domain.Role
}

// schemaDoc is a JSON Schema of a request body.
type schemaDoc struct {
	Ref                  string                     `json:"$ref"`
	Required             []string                   `json:"required"`
	Properties           map[string]json.RawMessage `json:"properties"`
	AdditionalProperties *json.RawMessage           `json:"additionalProperties"`
}

// parseSpec reads an OpenAPI document. An operation without an
// operationId or a valid access level is an error: the API fails closed.
func parseSpec(raw []byte) (spec, error) {
	var doc struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]schemaDoc `json:"schemas"`
		} `json:"components"`
	}

	if err := json.Unmarshal(raw, &doc); err != nil {
		return spec{}, fmt.Errorf("parse OpenAPI document: %w", err)
	}

	s := spec{bodies: bodyFields{}}

	for path, item := range doc.Paths {
		re, params := pathPattern(path)

		for method, rawOp := range item {
			method = strings.ToUpper(method)

			switch method {
			case http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete,
				http.MethodOptions, http.MethodHead, http.MethodPatch, http.MethodTrace:
			default:
				continue // parameters, summary, …
			}

			var op struct {
				ID          string `json:"operationId"`
				Access      string `json:"x-meshsdr-access"`
				RequestBody struct {
					Content map[string]struct {
						Schema schemaDoc `json:"schema"`
					} `json:"content"`
				} `json:"requestBody"`
			}

			if err := json.Unmarshal(rawOp, &op); err != nil {
				return spec{}, fmt.Errorf("%s %s: %w", method, path, err)
			}

			role, err := domain.ParseRole(op.Access)
			if err != nil || op.ID == "" {
				return spec{}, fmt.Errorf("%s %s: missing operationId or invalid %s %q", method, path, AccessExtension, op.Access)
			}

			s.operations = append(s.operations, operation{name: goName(op.ID), method: method, path: re, params: params, role: role})

			body, closed, err := closedBodyOf(op.RequestBody.Content, doc.Components.Schemas)
			if err != nil {
				return spec{}, fmt.Errorf("%s %s: %w", method, path, err)
			}

			if closed {
				s.bodies[method+" "+path] = body
			}
		}
	}

	slices.SortStableFunc(s.operations, func(a, b operation) int { return a.params - b.params })

	return s, nil
}

// pathPattern returns the regexp of an OpenAPI path and its number of
// parameters.
func pathPattern(path string) (*regexp.Regexp, int) {
	segments := strings.Split(path, "/")
	params := 0

	for i, seg := range segments {
		if strings.HasPrefix(seg, "{") {
			segments[i] = `[^/]+`
			params++
		} else {
			segments[i] = regexp.QuoteMeta(seg)
		}
	}

	return regexp.MustCompile("^" + strings.Join(segments, "/") + "$"), params
}

// closedBodyOf returns the closed JSON request body of an operation: its
// schema declares additionalProperties: false.
func closedBodyOf(content map[string]struct {
	Schema schemaDoc `json:"schema"`
}, schemas map[string]schemaDoc,
) (closedBody, bool, error) {
	media, ok := content["application/json"]
	if !ok {
		return closedBody{}, false, nil
	}

	s := media.Schema
	if name, ok := strings.CutPrefix(s.Ref, "#/components/schemas/"); ok {
		if s, ok = schemas[name]; !ok {
			return closedBody{}, false, fmt.Errorf("unknown schema %q", name)
		}
	}

	if s.AdditionalProperties == nil || string(*s.AdditionalProperties) != "false" {
		return closedBody{}, false, nil
	}

	allowed := map[string]bool{}
	for p := range s.Properties {
		allowed[p] = true
	}

	required := slices.Clone(s.Required)
	slices.Sort(required)

	return closedBody{allowed: allowed, required: required}, true, nil
}

// goName is the operation name oapi-codegen generates.
func goName(id string) string {
	r, n := utf8.DecodeRuneInString(id)

	return string(unicode.ToUpper(r)) + id[n:]
}

// match returns the operation of r (path with or without the /api/v1
// prefix: the handler is mounted under it), routed as method.
func (s spec) match(method string, r *http.Request) *operation {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")

	for i, op := range s.operations {
		if op.method == method && op.path.MatchString(path) {
			return &s.operations[i]
		}
	}

	return nil
}
