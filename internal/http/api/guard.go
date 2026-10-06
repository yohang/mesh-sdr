package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// JSONBodyLimit bounds every /api/v1 request body except the uploads, which
// have their own caps (uploadLimits).
const JSONBodyLimit = 1 << 20

// operation is an operation of openapi.yaml matched by method and path.
type operation struct {
	method    string
	path      *regexp.Regexp
	params    int
	role      domain.Role
	multipart bool
}

var (
	operationsOnce sync.Once
	operations     []operation
)

func specOperations() []operation {
	operationsOnce.Do(func() { operations = loadOperations(specJSON) })

	return operations
}

// loadOperations reads the operations of an OpenAPI document; the most
// specific paths (fewest parameters) come first. An operation without a
// valid access level is skipped here: the strict policy refuses it.
func loadOperations(spec []byte) []operation {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}

	if err := json.Unmarshal(spec, &doc); err != nil {
		return nil
	}

	var out []operation

	for path, item := range doc.Paths {
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

		re := regexp.MustCompile("^" + strings.Join(segments, "/") + "$")

		for method, raw := range item {
			var op struct {
				Access      string `json:"x-meshsdr-access"`
				RequestBody struct {
					Content map[string]json.RawMessage `json:"content"`
				} `json:"requestBody"`
			}

			if json.Unmarshal(raw, &op) != nil {
				continue
			}

			role, err := domain.ParseRole(op.Access)
			if err != nil {
				continue
			}

			_, multipart := op.RequestBody.Content["multipart/form-data"]
			out = append(out, operation{method: strings.ToUpper(method), path: re, params: params, role: role, multipart: multipart})
		}
	}

	slices.SortStableFunc(out, func(a, b operation) int { return a.params - b.params })

	return out
}

// matchOperation returns the operation of r (path with or without the
// /api/v1 prefix: the handler is mounted under it).
func matchOperation(r *http.Request) *operation {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")

	for i, op := range specOperations() {
		if op.method == r.Method && op.path.MatchString(path) {
			return &specOperations()[i]
		}
	}

	return nil
}

// AcceptsMultipart reports whether r targets an operation whose request
// body is declared multipart/form-data in openapi.yaml (uploads): the
// JSON-only rule of /api/v1 does not apply to it.
func AcceptsMultipart(r *http.Request) bool {
	op := matchOperation(r)

	return op != nil && op.multipart
}

// guard runs before routing: it checks the access level of the operation
// (x-meshsdr-access) before any body is read, so an unauthorised request
// never gets its body decoded, and bounds the body of every non-upload
// operation to JSONBodyLimit. The strict policy middleware checks the
// access level again after decoding (defence in depth).
func guard(authz Authorizer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			op := matchOperation(r)
			if op != nil {
				if err := authz.Authorize(r.Context(), op.role); err != nil {
					problem.Write(w, problem.FromError(err))

					return
				}
			}

			if op == nil || !op.multipart {
				r.Body = http.MaxBytesReader(w, r.Body, JSONBodyLimit)
			}

			next.ServeHTTP(w, r)
		})
	}
}
