package http

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// Login form messages (FEATURE_SPEC §10.3): one generic error, the same
// whether or not the account exists (SR-06).
const (
	msgInvalid   = "Incorrect username, e-mail or password."
	msgThrottled = "Too many attempts. Try again in %s."
	msgFailed    = "Sign in failed. Try again later."
)

// loginForm is the view of the login form.
type loginForm struct {
	Login    string
	Remember bool
	Next     string
	Error    string
}

// SafeNext returns next when it is a same-origin relative path to a known
// route (SR-12: `^/(?![/\\])`), otherwise "/". routes may be nil (no route
// check).
func SafeNext(next string, routes chi.Routes) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsAny(next, "\\\x00\r\n\t") {
		return "/"
	}

	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") ||
		strings.HasPrefix(u.Path, "//") {
		return "/"
	}

	if routes != nil && !routes.Match(chi.NewRouteContext(), http.MethodGet, u.Path) {
		return "/"
	}

	return u.RequestURI()
}

func noIndex(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
}

func (m *Module) loginPage(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	f := loginForm{Next: SafeNext(r.URL.Query().Get("next"), m.routes)}
	m.pages.Page(w, r, http.StatusOK, "Sign in", loginPage(f), nil)
}

func (m *Module) loginAction(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest

		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}

		m.pages.Error(w, r, status)

		return
	}

	f := loginForm{
		Login:    r.PostForm.Get("login"),
		Remember: r.PostForm.Get("remember_me") != "",
		Next:     SafeNext(r.PostForm.Get("next"), m.routes),
	}

	_, _, cookies, err := m.Login(r.Context(), f.Login, r.PostForm.Get("password"), f.Remember)
	if err == nil {
		for _, c := range cookies {
			http.SetCookie(w, c)
		}

		// Full page load: the new page fetches the new CSRF token.
		m.redirect(w, r, f.Next)

		return
	}

	status := http.StatusUnauthorized

	var rl *domain.RateLimitError

	switch {
	case errors.As(err, &rl):
		status = http.StatusTooManyRequests
		f.Error = fmt.Sprintf(msgThrottled, humanWait(rl.RetryAfter()))
		w.Header().Set("Retry-After", fmt.Sprint(int(rl.RetryAfter().Seconds())))
	case errors.Is(err, domain.ErrInvalidCredentials):
		f.Error = msgInvalid
	default:
		status = http.StatusInternalServerError
		f.Error = msgFailed

		m.logger.ErrorContext(r.Context(), "login failed", slog.Any("error", err))
	}

	m.pages.Page(w, r, status, "Sign in", loginPage(f), loginFormView(f))
}

func humanWait(d time.Duration) string {
	if d < time.Minute {
		n := int(math.Ceil(d.Seconds()))
		if n == 1 {
			return "1 second"
		}

		return fmt.Sprintf("%d seconds", n)
	}

	n := int(math.Ceil(d.Minutes()))
	if n == 1 {
		return "1 minute"
	}

	return fmt.Sprintf("%d minutes", n)
}

func (m *Module) logoutAction(w http.ResponseWriter, r *http.Request) {
	c, err := m.Logout(r.Context())
	if err != nil {
		m.logger.ErrorContext(r.Context(), "logout failed", slog.Any("error", err))
		m.pages.Error(w, r, http.StatusInternalServerError)

		return
	}

	http.SetCookie(w, c)
	m.redirect(w, r, "/")
}
