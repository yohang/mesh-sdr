// Package apitest checks the hub REST API against its OpenAPI document in
// tests (API-002 contract tests, ADR 0013). It wraps a handler: every
// response under /api/v1 is validated against openapi.yaml (status, headers,
// content type and body schema), every request the server accepts (2xx) is
// validated too, and the operations exercised are recorded so a suite can
// require that each one answered a validated 2xx at least once.
//
// It uses kin-openapi, which is a build- and test-time dependency only:
// depguard keeps it out of the meshsdr binary (it may be imported only by
// this package, specgen and _test.go files).
package apitest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
)

// Prefix is the path prefix of the validated API.
const Prefix = "/api/v1"

// Operation is one operation of the document.
type Operation struct {
	ID     string
	Method string
	// Path is the templated path, without the /api/v1 prefix.
	Path string
	// Access is the x-meshsdr-access level.
	Access string
	// HasBody tells whether the operation declares a request body.
	HasBody bool
	// MinimalBody is a JSON body with a placeholder for each required
	// property of the JSON request body schema (nil without a JSON body).
	MinimalBody map[string]any
}

// Reporter receives contract violations (testing.TB satisfies it).
type Reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// Validator validates exchanges against the document.
type Validator struct {
	doc    *openapi3.T
	router routers.Router
	ops    []Operation

	mu      sync.Mutex
	success map[string]bool
	seen    map[string]bool
}

// New loads the OpenAPI document (JSON or YAML) and validates it.
func New(spec []byte) (*Validator, error) {
	doc, err := openapi3.NewLoader().LoadFromData(spec)
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI document: %w", err)
	}

	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("validate OpenAPI document: %w", err)
	}

	// Route on paths relative to the API prefix, whatever the servers say.
	doc.Servers = openapi3.Servers{{URL: "/"}}

	router, err := legacy.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("OpenAPI router: %w", err)
	}

	v := &Validator{doc: doc, router: router, success: map[string]bool{}, seen: map[string]bool{}}

	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			access, _ := op.Extensions["x-meshsdr-access"].(string)
			o := Operation{ID: op.OperationID, Method: method, Path: path, Access: access, HasBody: op.RequestBody != nil}
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				if media := op.RequestBody.Value.Content.Get("application/json"); media != nil && media.Schema != nil {
					o.MinimalBody = minimal(media.Schema.Value)
				}
			}

			v.ops = append(v.ops, o)
		}
	}

	sort.Slice(v.ops, func(i, j int) bool { return v.ops[i].ID < v.ops[j].ID })

	return v, nil
}

// Operations returns every operation of the document, by id.
func (v *Validator) Operations() []Operation { return slices.Clone(v.ops) }

// Uncovered returns the ids of the operations that never answered a
// validated 2xx through the validator.
func (v *Validator) Uncovered() []string {
	v.mu.Lock()
	defer v.mu.Unlock()

	var out []string

	for _, op := range v.ops {
		if !v.success[op.ID] {
			out = append(out, op.ID)
		}
	}

	return out
}

// Handler returns next wrapped with the validation of every exchange under
// /api/v1; violations go to r.
func (v *Validator) Handler(r Reporter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.URL.Path, Prefix+"/") {
			next.ServeHTTP(w, req)

			return
		}

		reqBody, err := io.ReadAll(req.Body)
		if err != nil {
			r.Errorf("read request body of %s %s: %v", req.Method, req.URL.Path, err)
		}

		req.Body = io.NopCloser(bytes.NewReader(reqBody))

		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, req)

		v.check(r, req, reqBody, rec)

		for k, vals := range rec.Header() {
			w.Header()[k] = vals
		}

		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

func (v *Validator) check(r Reporter, req *http.Request, reqBody []byte, rec *httptest.ResponseRecorder) {
	r.Helper()

	where := req.Method + " " + req.URL.Path

	// Route on a copy whose path is relative to the API prefix.
	routed := req.Clone(req.Context())
	routed.URL.Path = strings.TrimPrefix(req.URL.Path, Prefix)
	routed.URL.RawPath = ""
	routed.Body = io.NopCloser(bytes.NewReader(reqBody))

	route, params, err := v.router.FindRoute(routed)
	if err != nil {
		// Unknown paths and methods must still answer a problem.
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			r.Errorf("%s: no operation (%v) and the answer %d is %q, not a problem", where, err, rec.Code, ct)
		}

		return
	}

	opts := &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, IncludeResponseStatus: true}
	in := &openapi3filter.RequestValidationInput{Request: routed, PathParams: params, Route: route, Options: opts}
	reqErr := openapi3filter.ValidateRequest(req.Context(), in)

	ok := rec.Code >= 200 && rec.Code < 300
	if ok && reqErr != nil {
		r.Errorf("%s: the server accepted (%d) a request the document refuses: %v", where, rec.Code, reqErr)
	}

	// Validate the response against the request that was sent.
	in.Request = req.Clone(req.Context())
	in.Request.URL.Path = routed.URL.Path
	in.Request.Body = io.NopCloser(bytes.NewReader(reqBody))

	res := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: rec.Code, Header: rec.Header(), Options: opts}
	res.SetBodyBytes(rec.Body.Bytes())

	if err := openapi3filter.ValidateResponse(req.Context(), res); err != nil {
		r.Errorf("%s: response %d does not match the document: %v\n%s", where, rec.Code, err, rec.Body.String())
	}

	if rec.Code >= 400 && !isProblem(rec) {
		r.Errorf("%s: error %d is not a problem+json with a code: %s", where, rec.Code, rec.Body.String())
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	v.seen[route.Operation.OperationID] = true
	if ok {
		v.success[route.Operation.OperationID] = true
	}
}

// isProblem tells whether rec is an RFC 9457 problem with a stable code
// (the API's one error format).
func isProblem(rec *httptest.ResponseRecorder) bool {
	if rec.Header().Get("Content-Type") != "application/problem+json" {
		return false
	}

	var p struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}

	return json.Unmarshal(rec.Body.Bytes(), &p) == nil && p.Status == rec.Code && p.Code != ""
}

// minimal returns an object with a placeholder for each required property.
func minimal(s *openapi3.Schema) map[string]any {
	out := map[string]any{}
	if s == nil {
		return out
	}

	for _, name := range s.Required {
		var prop *openapi3.Schema
		if ref := s.Properties[name]; ref != nil {
			prop = ref.Value
		}

		out[name] = placeholder(prop)
	}

	return out
}

func placeholder(s *openapi3.Schema) any {
	switch {
	case s == nil || s.Type == nil:
		return "x"
	case s.Type.Is("integer"), s.Type.Is("number"):
		return 0
	case s.Type.Is("boolean"):
		return false
	case s.Type.Is("array"):
		return []any{}
	case s.Type.Is("object"):
		return minimal(s)
	default:
		if len(s.Enum) > 0 {
			return s.Enum[0]
		}

		return "x"
	}
}
