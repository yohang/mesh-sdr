package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Response writers and helpers shared by the handlers.

// jsonOK writes a 200 JSON body, not cached.
type jsonOK struct{ v any }

func (j jsonOK) write(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	return json.NewEncoder(w).Encode(j.v)
}

// rateLimited is a 429 problem with Retry-After.
type rateLimited struct{ err *domain.RateLimitError }

func (r rateLimited) write(w http.ResponseWriter) error {
	w.Header().Set("Retry-After", strconv.Itoa(int(r.err.RetryAfter().Seconds())))
	problem.Write(w, problem.FromError(r.err))

	return nil
}
