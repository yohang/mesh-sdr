// Package problem writes the API's single JSON error format: RFC 9457
// problem details with a stable code (TECHNICAL_SPEC §6.10), served as
// application/problem+json, and maps domain errors to it.
package problem

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ContentType is the media type of problem responses.
const ContentType = "application/problem+json"

// Generic codes used outside the domain.
const (
	CodeBadRequest       = "bad_request"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternal         = "internal_error"
	CodeForbidden        = "forbidden"
	CodeNotImplemented   = "not_implemented"
)

// FieldError is one invalid field of a request.
type FieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Problem is an RFC 9457 problem details object.
type Problem struct {
	Type   string       `json:"type"`
	Title  string       `json:"title"`
	Status int          `json:"status"`
	Code   string       `json:"code"`
	Detail string       `json:"detail,omitempty"`
	Errors []FieldError `json:"errors,omitempty"`
}

// New returns a problem for status with a stable code and a detail.
func New(status int, code, detail string) Problem {
	return Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Code: code, Detail: detail}
}

// Write sends p.
func Write(w http.ResponseWriter, p Problem) {
	w.Header().Set("Content-Type", ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// StatusOf maps a domain error kind to an HTTP status.
func StatusOf(k shared.Kind) int {
	switch k {
	case shared.KindInvalid:
		return http.StatusUnprocessableEntity
	case shared.KindNotFound:
		return http.StatusNotFound
	case shared.KindConflict:
		return http.StatusConflict
	case shared.KindForbidden:
		return http.StatusForbidden
	case shared.KindUnauthenticated:
		return http.StatusUnauthorized
	case shared.KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// FromError maps err to a problem. Domain errors keep their code, message and
// violations; any other error is an opaque 500 internal_error (its details
// must not leak).
func FromError(err error) Problem {
	var de *shared.Error
	if errors.As(err, &de) {
		p := New(StatusOf(de.Kind()), string(de.Code()), de.Message())
		for _, v := range de.Violations() {
			p.Errors = append(p.Errors, FieldError{Path: v.Path(), Code: string(v.Code()), Message: v.Message()})
		}

		return p
	}

	return New(http.StatusInternalServerError, CodeInternal, "")
}

// ErrorHandler returns a handler for errors raised while serving r: it logs
// the error once (Error for 5xx, Debug otherwise) and writes the problem.
func ErrorHandler(logger *slog.Logger) func(w http.ResponseWriter, r *http.Request, err error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		p := FromError(err)

		level := slog.LevelDebug
		if p.Status >= http.StatusInternalServerError {
			level = slog.LevelError
		}

		logger.LogAttrs(r.Context(), level, "api request failed",
			slog.String("method", r.Method), slog.String("path", r.URL.Path),
			slog.Int("status", p.Status), slog.String("code", p.Code), slog.Any("error", err))

		Write(w, p)
	}
}

// BadRequest handles request decoding errors (malformed body or parameters).
func BadRequest(w http.ResponseWriter, _ *http.Request, err error) {
	Write(w, New(http.StatusBadRequest, CodeBadRequest, err.Error()))
}

// NotFound is a chi NotFound handler.
func NotFound(w http.ResponseWriter, _ *http.Request) {
	Write(w, New(http.StatusNotFound, CodeNotFound, ""))
}

// MethodNotAllowed is a chi MethodNotAllowed handler.
func MethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	Write(w, New(http.StatusMethodNotAllowed, CodeMethodNotAllowed, ""))
}

// Recoverer turns a panic into a logged 500 problem.
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}

				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared as in net/http
					panic(rec)
				}

				logger.ErrorContext(r.Context(), "panic serving api request",
					slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("panic", rec))
				Write(w, New(http.StatusInternalServerError, CodeInternal, ""))
			}()

			next.ServeHTTP(w, r)
		})
	}
}
