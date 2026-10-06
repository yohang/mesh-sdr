package http

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/http/clientip"
	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

const (
	pageTitleSetup = "Create the first admin"
	msgSetupFailed = "The account could not be created. Try again later."
)

// setupForm is the view of the first-admin form.
type setupForm struct {
	Token       string
	Username    string
	Email       string
	DisplayName string
	MinLength   int
	Error       string
	// Field is the field the error is about: username, email,
	// display_name or password.
	Field string
}

// setupHeaders: the setup page carries a single-use token, so it is never
// cached, indexed or sent as a referrer (SR-07, SR-28).
func setupHeaders(w http.ResponseWriter) {
	noIndex(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// setupAllowed checks the client address against admin.allowed_networks:
// the setup creates an admin.
func (m *Module) setupAllowed(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case m.setup == nil:
		m.pages.Error(w, r, http.StatusNotFound)

		return false
	case !clientip.In(clientip.From(r.Context()), m.admin):
		m.logger.WarnContext(r.Context(), "setup refused outside admin.allowed_networks")
		m.pages.Error(w, r, http.StatusForbidden)

		return false
	}

	return true
}

// setupLanding answers GET /setup, the address the setup page shows once it
// has hidden its token: a reload cannot recover the token.
func (m *Module) setupLanding(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)

	if m.setupAllowed(w, r) {
		m.setupRefused(w, r, domain.ErrSetupTokenInvalid)
	}
}

func (m *Module) setupPage(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)

	if !m.setupAllowed(w, r) {
		return
	}

	token := chi.URLParam(r, "token")

	if err := m.setup.Check(token, m.meta(r.Context())); err != nil {
		m.setupRefused(w, r, err)

		return
	}

	f := setupForm{Token: token, MinLength: m.setup.MinLength(r.Context())}
	m.pages.Page(w, r, http.StatusOK, pageTitleSetup, setupPage(f), nil)
}

func (m *Module) setupAction(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)

	if !m.setupAllowed(w, r) {
		return
	}

	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest

		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}

		m.pages.Error(w, r, status)

		return
	}

	f := setupForm{
		Token:       r.PostForm.Get("token"),
		Username:    r.PostForm.Get("username"),
		Email:       r.PostForm.Get("email"),
		DisplayName: r.PostForm.Get("display_name"),
		MinLength:   m.setup.MinLength(r.Context()),
	}

	password := r.PostForm.Get("password")
	if password != r.PostForm.Get("confirm_password") {
		f.Error, f.Field = "The passwords do not match.", "password"
		m.pages.Page(w, r, http.StatusUnprocessableEntity, pageTitleSetup, setupPage(f), setupFormView(f))

		return
	}

	previous := ""
	if st := FromContext(r.Context()); st.session != nil {
		previous = st.token.Cookie()
	}

	res, err := m.setup.Complete(r.Context(), app.SetupInput{
		Token: f.Token, Username: f.Username, Email: f.Email, DisplayName: f.DisplayName, Password: password,
		Previous: previous, Meta: m.meta(r.Context()),
	})
	if err == nil {
		for _, c := range m.sessionCookies(res) {
			http.SetCookie(w, c)
		}

		m.redirect(w, r, "/")

		return
	}

	var de *shared.Error

	switch {
	case errors.Is(err, domain.ErrSetupTokenInvalid), errors.Is(err, domain.ErrRateLimited):
		m.setupRefused(w, r, err)

		return
	case errors.Is(err, domain.ErrInvalidUsername), errors.Is(err, domain.ErrUsernameTaken):
		f.Field = "username"
	case errors.Is(err, domain.ErrInvalidEmail), errors.Is(err, domain.ErrEmailTaken):
		f.Field = "email"
	case errors.Is(err, domain.ErrInvalidDisplayName):
		f.Field = "display_name"
	case errors.Is(err, domain.ErrInvalidPassword):
		f.Field = "password"
	}

	status := http.StatusUnprocessableEntity

	if f.Field != "" && errors.As(err, &de) {
		f.Error = sentence(de.Message())
	} else {
		status = http.StatusInternalServerError
		f.Error = msgSetupFailed

		m.logger.ErrorContext(r.Context(), "first admin setup failed", slog.Any("error", err))
	}

	m.pages.Page(w, r, status, pageTitleSetup, setupPage(f), setupFormView(f))
}

// setupRefused shows why the link cannot be used: invalid (404) or too many
// requests (429).
func (m *Module) setupRefused(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := http.StatusNotFound, ""

	var rl *domain.RateLimitError
	if errors.As(err, &rl) {
		status = http.StatusTooManyRequests
		msg = fmt.Sprintf(msgThrottled, humanWait(rl.RetryAfter()))
		w.Header().Set("Retry-After", fmt.Sprint(int(rl.RetryAfter().Seconds())))
	}

	m.pages.Page(w, r, status, pageTitleSetup, setupInvalid(msg), nil)
}
