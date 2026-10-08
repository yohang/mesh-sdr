package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	h := NewHandler(Server{}, deny, slog.New(slog.DiscardHandler))

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
	h := NewHandler(Server{}, allow, slog.New(slog.DiscardHandler))

	body := &countingBody{}
	req := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	req.Body = io.NopCloser(io.MultiReader(strings.NewReader(`{"node_id":"`), body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge || body.n > 2*JSONBodyLimit {
		t.Errorf("oversize POST = %d, %d bytes read", rec.Code, body.n)
	}
}
