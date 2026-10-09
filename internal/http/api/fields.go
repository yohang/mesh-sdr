package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/http/problem"
)

// bodyFields maps an operation ("METHOD /path" as in the OpenAPI document)
// to the fields its JSON request body may carry (the properties of a body
// schema that declares additionalProperties: false) and must carry (its
// required properties).
type bodyFields map[string]closedBody

// closedBody is the closed request body of an operation.
type closedBody struct {
	allowed  map[string]bool
	required []string
}

// rejectUnknownFields refuses JSON bodies with fields their schema does not
// declare (SR-20): 400 unknown_field, one field error per unknown field. It
// also refuses bodies without a field their schema requires (400
// missing_field), which the generated decoder would otherwise read as its
// zero value (an absent display name would clear it).
// It runs once the route is matched, after the guard refused the callers
// the access level does not allow, and restores the body for the
// generated decoder.
func (f bodyFields) rejectUnknownFields(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := f.of(r)
		if !ok || r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)

			return
		}

		raw, err := io.ReadAll(r.Body)
		if err != nil {
			problem.BadRequest(w, r, err)

			return
		}

		r.Body = io.NopCloser(bytes.NewReader(raw))

		var obj map[string]json.RawMessage
		if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &obj) != nil {
			next.ServeHTTP(w, r) // the generated decoder reports malformed bodies

			return
		}

		var unknown []string

		for k := range obj {
			if !body.allowed[k] {
				unknown = append(unknown, k)
			}
		}

		var missing []string

		for _, k := range body.required {
			if _, ok := obj[k]; !ok {
				missing = append(missing, k)
			}
		}

		if len(unknown) == 0 && len(missing) == 0 {
			next.ServeHTTP(w, r)

			return
		}

		if len(unknown) == 0 {
			p := problem.New(http.StatusBadRequest, "missing_field", "the request body lacks required fields")
			for _, k := range missing {
				p.Errors = append(p.Errors, problem.FieldError{Path: k, Code: "missing_field", Message: "required field"})
			}

			problem.Write(w, p)

			return
		}

		slices.Sort(unknown)

		p := problem.New(http.StatusBadRequest, "unknown_field", "the request body has fields that the API does not define")
		for _, k := range unknown {
			p.Errors = append(p.Errors, problem.FieldError{Path: k, Code: "unknown_field", Message: "unknown field"})
		}

		problem.Write(w, p)
	})
}

// of returns the closed body of the request's operation, if it has one.
func (f bodyFields) of(r *http.Request) (closedBody, bool) {
	rc := chi.RouteContext(r.Context())
	if rc == nil {
		return closedBody{}, false
	}

	pattern := rc.RoutePattern()

	if b, ok := f[r.Method+" "+pattern]; ok {
		return b, true
	}

	// Mounted under /api/v1 by the router.
	if i := strings.Index(pattern, "/v1/"); i >= 0 {
		b, ok := f[r.Method+" "+pattern[i+len("/v1"):]]

		return b, ok
	}

	return closedBody{}, false
}
