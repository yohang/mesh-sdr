package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Response writers and helpers shared by the handlers.

// jsonOK writes a 200 JSON body, not cached, with the cookies to set and,
// for a download, its file name.
type jsonOK struct {
	v        any
	cookies  []*http.Cookie
	download string
	indent   bool
}

func (j jsonOK) write(w http.ResponseWriter) error {
	for _, c := range j.cookies {
		http.SetCookie(w, c)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")

	enc := json.NewEncoder(w)

	if j.download != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+j.download+`"`)
	}

	if j.indent {
		enc.SetIndent("", "  ")
	}

	w.WriteHeader(http.StatusOK)

	return enc.Encode(j.v)
}

// rateLimited is a 429 problem with Retry-After.
type rateLimited struct{ err *domain.RateLimitError }

func (r rateLimited) write(w http.ResponseWriter) error {
	w.Header().Set("Retry-After", strconv.Itoa(int(r.err.RetryAfter().Seconds())))
	problem.Write(w, problem.FromError(r.err))

	return nil
}
