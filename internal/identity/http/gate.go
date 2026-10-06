package http

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/yohang/mesh-sdr/internal/http/redact"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// PasswordChangePath is the password change page (AUTH-006, AUTH-007).
const PasswordChangePath = "/account/password"

// pendingAllowed are the paths a user flagged must_change_password may
// still reach: the password change itself, logout, the session API (CSRF
// token), and the static resources and health checks that pages need.
var pendingAllowed = map[string]bool{
	PasswordChangePath:      true,
	"/logout":               true,
	"/policy":               true,
	"/favicon.ico":          true,
	"/manifest.webmanifest": true,
	"/robots.txt":           true,
	"/api/v1/auth/session":  true,
	"/api/v1/auth/logout":   true,
	"/api/v1/auth/password": true,
	"/api/v1/openapi.json":  true,
}

var pendingAllowedPrefixes = []string{"/static/", "/api/v1/healthz/"}

func allowedWhilePending(path string) bool {
	if pendingAllowed[path] {
		return true
	}

	for _, p := range pendingAllowedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}

	return false
}

// passwordGate confines a user flagged must_change_password to the password
// change (AUTH-006): every other route, anonymous ones included, redirects
// pages to the change page (with a safe next) and refuses API calls with
// 403 password_change_required.
func (m *Module) passwordGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := FromContext(r.Context()).Principal()
		// chi routes on the raw path when it differs from the decoded one:
		// such a request is never allow-listed.
		allowed := allowedWhilePending(r.URL.Path) && (r.URL.RawPath == "" || r.URL.RawPath == r.URL.Path)

		if p.IsAnonymous() || !p.MustChangePassword() || allowed {
			next.ServeHTTP(w, r)

			return
		}

		m.logger.DebugContext(r.Context(), "password change required", slog.String("path", redact.Path(r.URL.Path)))

		if isAPI(r) {
			m.deny(w, r, http.StatusForbidden, domain.ErrPasswordChangeRequired)

			return
		}

		to := PasswordChangePath + "?forced=1"

		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if n := SafeNext(r.URL.RequestURI(), m.routes); n != "/" {
				to += "&next=" + url.QueryEscape(n)
			}
		}

		m.redirect(w, r, to)
	})
}
