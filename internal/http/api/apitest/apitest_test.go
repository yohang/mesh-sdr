package apitest_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/http/api/apitest"
)

const spec = `openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /things/{id}:
    parameters:
      - {name: id, in: path, required: true, schema: {type: string}}
    get:
      operationId: getThing
      x-meshsdr-access: anonymous
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                required: [id]
                properties: {id: {type: string}}
        default:
          description: error
          content:
            application/problem+json:
              schema: {type: object}
  /other:
    get:
      operationId: getOther
      x-meshsdr-access: admin
      responses:
        "204": {description: ok}
`

// recorder collects violations.
type recorder struct{ errs []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func TestValidator(t *testing.T) {
	v, err := apitest.New([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}

	if ops := v.Operations(); len(ops) != 2 || ops[0].ID != "getOther" || ops[0].Access != "admin" || ops[1].Path != "/things/{id}" {
		t.Fatalf("operations = %+v", ops)
	}

	tests := []struct {
		name, path string
		status     int
		ctype      string
		body       string
		violation  string
	}{
		{"valid", "/api/v1/things/a", 200, "application/json", `{"id":"a"}`, ""},
		{"wrong body", "/api/v1/things/a", 200, "application/json", `{"name":"a"}`, "does not match"},
		{"error as plain text", "/api/v1/things/a", 500, "text/plain", "boom", "not a problem"},
		{"problem", "/api/v1/things/a", 404, "application/problem+json", `{"status":404,"code":"not_found"}`, ""},
		{"unknown path as HTML", "/api/v1/nope", 404, "text/html", "<p>", "no operation"},
		{"outside the API", "/page", 200, "text/html", "<p>", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			h := v.Handler(rec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.ctype)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))

			out := httptest.NewRecorder()
			h.ServeHTTP(out, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if out.Code != tt.status || out.Body.String() != tt.body {
				t.Errorf("response not passed through: %d %q", out.Code, out.Body.String())
			}

			got := strings.Join(rec.errs, "\n")
			if (tt.violation == "") != (got == "") || !strings.Contains(got, tt.violation) {
				t.Errorf("violations = %q, want %q", got, tt.violation)
			}
		})
	}

	if got := v.Uncovered(); !slices.Equal(got, []string{"getOther"}) {
		t.Errorf("uncovered = %v", got)
	}
}
