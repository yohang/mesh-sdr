package problem_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yohang/mesh-sdr/internal/http/problem"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestFromError(t *testing.T) {
	errTaken := shared.NewError(shared.KindConflict, "username_taken", "username is taken")

	tests := []struct {
		name   string
		err    error
		status int
		code   string
		detail string
		fields int
	}{
		{"domain error", fmt.Errorf("create user: %w", errTaken), http.StatusConflict, "username_taken", "username is taken", 0},
		{"invalid with violations", shared.NewError(shared.KindInvalid, "invalid_input", "bad input").
			WithViolations(shared.NewViolation("username", "required", "username is required")), http.StatusUnprocessableEntity, "invalid_input", "bad input", 1},
		{"not found", shared.NewError(shared.KindNotFound, "node_not_found", "x"), http.StatusNotFound, "node_not_found", "x", 0},
		{"forbidden", shared.NewError(shared.KindForbidden, "f", ""), http.StatusForbidden, "f", "", 0},
		{"unauthenticated", shared.NewError(shared.KindUnauthenticated, "u", ""), http.StatusUnauthorized, "u", "", 0},
		{"unavailable", shared.NewError(shared.KindUnavailable, "v", ""), http.StatusServiceUnavailable, "v", "", 0},
		{"rate limited", shared.NewError(shared.KindRateLimited, "rate_limited", ""), http.StatusTooManyRequests, "rate_limited", "", 0},
		{"infra error does not leak", errors.New("sqlite: disk I/O error at /var/lib"), http.StatusInternalServerError, "internal_error", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := problem.FromError(tt.err)
			if p.Status != tt.status || p.Code != tt.code || p.Detail != tt.detail || len(p.Errors) != tt.fields || p.Type != "about:blank" || p.Title != http.StatusText(tt.status) {
				t.Fatalf("got %+v", p)
			}
		})
	}
}

func TestErrorHandlerAndRecoverer(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	rec := httptest.NewRecorder()
	problem.ErrorHandler(logger)(rec, httptest.NewRequest(http.MethodGet, "/x", nil), errors.New("boom"))

	var p problem.Problem
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}

	if rec.Code != 500 || rec.Header().Get("Content-Type") != problem.ContentType || p.Code != problem.CodeInternal {
		t.Fatalf("got %d %+v", rec.Code, p)
	}

	rec = httptest.NewRecorder()
	h := problem.Recoverer(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("oops") }))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != 500 || rec.Header().Get("Content-Type") != problem.ContentType {
		t.Fatalf("panic: got %d", rec.Code)
	}
}
