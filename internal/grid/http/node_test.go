package http_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
)

func TestPreEnrollmentRouter(t *testing.T) {
	h := gridhttp.NewPreEnrollmentRouter(nil, slog.New(slog.DiscardHandler))

	tests := []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodPost, "/enroll", http.StatusNotImplemented, "not_implemented"},
		{http.MethodGet, "/enroll", http.StatusForbidden, "forbidden"},
		{http.MethodGet, "/ws", http.StatusForbidden, "forbidden"},
		{http.MethodGet, "/control", http.StatusForbidden, "forbidden"},
		{http.MethodGet, "/healthz/live", http.StatusForbidden, "forbidden"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}

			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("content type = %q", ct)
			}

			var body struct {
				Code   string `json:"code"`
				Status int    `json:"status"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}

			if body.Code != tt.code || body.Status != tt.status {
				t.Errorf("body = %+v", body)
			}
		})
	}
}
