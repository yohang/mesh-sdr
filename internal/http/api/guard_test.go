package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// countingBody is an endless body that counts the bytes read from it.
type countingBody struct{ n int64 }

func (b *countingBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}

	b.n += int64(len(p))

	return len(p), nil
}

func (b *countingBody) Close() error { return nil }

type authzFunc func(ctx context.Context, role domain.Role) error

func (f authzFunc) Authorize(ctx context.Context, role domain.Role) error { return f(ctx, role) }

func TestGuardRefusesBeforeReadingTheBody(t *testing.T) {
	deny := authzFunc(func(_ context.Context, role domain.Role) error {
		if role == domain.RoleAnonymous {
			return nil
		}

		return domain.ErrUnauthenticated
	})
	h := mustHandler(t, deny)

	body := &countingBody{}
	req := httptest.NewRequest(http.MethodGet, "/config/effective", nil)
	req.Body = body
	req.ContentLength = 64 << 20
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || body.n != 0 {
		t.Errorf("anonymous oversize request = %d, %d bytes read", rec.Code, body.n)
	}
}

func TestGuardBoundsJSONBodies(t *testing.T) {
	allow := authzFunc(func(context.Context, domain.Role) error { return nil })
	h := mustHandler(t, allow)

	body := &countingBody{}
	req := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	req.Body = io.NopCloser(io.MultiReader(strings.NewReader(`{"node_id":"`), body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge || body.n > 2*testMaxBody {
		t.Errorf("oversize POST = %d, %d bytes read", rec.Code, body.n)
	}
}

const testMaxBody = 1 << 20

func mustHandler(t *testing.T, authz Authorizer) http.Handler {
	t.Helper()

	h, err := NewHandler(Server{}, authz, testMaxBody, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	return h
}

// The guard is the one access check: an anonymous caller gets 401
// unauthenticated, a caller without the role 403 forbidden, before any
// handler runs, and a HEAD request served by its GET route (chi GetHead in
// the hub router) is checked as the GET operation.
func TestGuardProblemCodes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		method string
		err    error
		status int
		code   string
	}{
		{"unauthenticated", http.MethodGet, domain.ErrUnauthenticated, http.StatusUnauthorized, "unauthenticated"},
		{"forbidden", http.MethodGet, domain.ErrForbidden, http.StatusForbidden, "forbidden"},
		{"head forbidden", http.MethodHead, domain.ErrForbidden, http.StatusForbidden, "forbidden"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var asked domain.Role

			h := mustHandler(t, authzFunc(func(_ context.Context, role domain.Role) error {
				asked = role
				if role == domain.RoleAnonymous {
					return nil
				}

				return tt.err
			}))

			root := chi.NewRouter()
			root.Use(middleware.GetHead)
			root.Mount("/api/v1", h)

			rec := httptest.NewRecorder()
			root.ServeHTTP(rec, httptest.NewRequest(tt.method, "/api/v1/config/effective", nil))

			if rec.Code != tt.status || asked != domain.RoleAdmin {
				t.Fatalf("status = %d, role asked %v", rec.Code, asked)
			}

			if tt.method == http.MethodHead {
				return
			}

			var p struct {
				Code string `json:"code"`
			}

			if err := json.NewDecoder(rec.Body).Decode(&p); err != nil || p.Code != tt.code {
				t.Errorf("problem code = %q, %v, want %q", p.Code, err, tt.code)
			}
		})
	}
}
