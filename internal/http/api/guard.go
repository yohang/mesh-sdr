package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/http/problem"
)

// guard runs before routing: it checks the access level of the operation
// (x-meshsdr-access) before any body is read, so an unauthorised request
// never gets its body decoded, and bounds every body to maxBody. It is the
// one access check of the API and fails closed: a request that matches no
// operation is answered 404 (405 with an Allow header for a known path)
// before routing, and parseSpec refuses an operation without an access
// level.
func (s spec) guard(authz Authorizer, maxBody int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			op, allowed := s.match(routeMethod(r), r)

			switch {
			case op == nil && len(allowed) > 0:
				// RFC 9110 §15.5.6: a 405 lists the methods the path
				// accepts.
				w.Header().Set("Allow", strings.Join(allowed, ", "))
				problem.MethodNotAllowed(w, r)

				return
			case op == nil:
				// No operation: never route a request whose access was not
				// checked.
				problem.NotFound(w, r)

				return
			}

			if err := authz.Authorize(r.Context(), op.role); err != nil {
				problem.Write(w, problem.FromError(err))

				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, maxBody)

			next.ServeHTTP(w, r)
		})
	}
}

// routeMethod is the method the request is routed as: a HEAD request
// without a HEAD route is served by its GET route (chi
// middleware.GetHead), so its access is the GET operation's.
func routeMethod(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RouteMethod != "" {
		return rctx.RouteMethod
	}

	return r.Method
}
