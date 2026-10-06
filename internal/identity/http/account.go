package http

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// AccountPath is the account page (ACC-004).
const AccountPath = "/account"

const (
	pageTitleAccount = "Account"
	msgSaveFailed    = "The change could not be saved. Try again later."
)

// ShellUser returns the signed-in user of a request for the top bar, nil
// when anonymous.
func (m *Module) ShellUser(r *http.Request) *layout.User {
	p := m.Principal(r.Context())
	if p.IsAnonymous() {
		return nil
	}

	name := p.Username().String()
	if d := p.DisplayName(); !d.IsZero() {
		name = d.String()
	}

	return &layout.User{Name: name, Links: []layout.Link{{Label: "Account", Href: AccountPath}}}
}

// notice is the outcome message of a section form.
type notice struct {
	Text  string
	Error bool
	// Field is the input the error is about.
	Field string
}

// accountView is the account page.
type accountView struct {
	User        *domain.User
	MailEnabled bool
	Sessions    []app.SessionView
	Profile     notice
	Email       notice
	SessionsMsg notice
	Data        notice
}

func (m *Module) accountView(r *http.Request) (accountView, error) {
	ctx := r.Context()
	by := m.Actor(ctx)

	u, err := m.profile.Me(ctx, by)
	if err != nil {
		return accountView{}, err
	}

	sessions, err := m.accounts.OwnSessions(ctx, by)
	if err != nil {
		return accountView{}, err
	}

	return accountView{User: u, MailEnabled: m.profile.MailEnabled(), Sessions: sessions}, nil
}

func (m *Module) accountPage(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	v, err := m.accountView(r)
	if err != nil {
		m.pageFailed(w, r, err)

		return
	}

	m.pages.Page(w, r, http.StatusOK, pageTitleAccount, accountPage(v), nil)
}

// pageFailed answers an unexpected error of a page.
func (m *Module) pageFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, domain.ErrUnauthenticated) {
		m.redirect(w, r, "/login")

		return
	}

	m.logger.ErrorContext(r.Context(), "page failed", slog.Any("error", err))
	m.pages.Error(w, r, http.StatusInternalServerError)
}

// formError turns an error into a section notice and its status. Unknown
// errors are logged and shown as failed.
func (m *Module) formError(r *http.Request, err error, field string) (notice, int) {
	var (
		rl *domain.RateLimitError
		de *shared.Error
	)

	switch {
	case errors.As(err, &rl):
		return notice{Text: fmt.Sprintf(msgThrottled, humanWait(rl.RetryAfter())), Error: true, Field: "current_password"}, http.StatusTooManyRequests
	case errors.Is(err, domain.ErrInvalidCurrentPassword):
		return notice{Text: msgCurrentIncorrect, Error: true, Field: "current_password"}, http.StatusUnprocessableEntity
	case errors.As(err, &de) && de.Kind() != shared.KindUnavailable:
		return notice{Text: sentence(de.Message()), Error: true, Field: field}, http.StatusUnprocessableEntity
	}

	m.logger.ErrorContext(r.Context(), "account change failed", slog.Any("error", err))

	return notice{Text: msgSaveFailed, Error: true}, http.StatusInternalServerError
}

// section answers a section form: the section alone for htmx, otherwise
// the whole page.
func (m *Module) section(w http.ResponseWriter, r *http.Request, status int, v accountView, fragment func(accountView) templ.Component) {
	m.pages.Page(w, r, status, pageTitleAccount, accountPage(v), fragment(v))
}

func (m *Module) profileAction(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	if !m.parseForm(w, r) {
		return
	}

	_, err := m.profile.SetDisplayName(r.Context(), m.Actor(r.Context()), r.PostForm.Get("display_name"))

	v, verr := m.accountView(r)
	if verr != nil {
		m.pageFailed(w, r, verr)

		return
	}

	status := http.StatusOK
	if err != nil {
		v.Profile, status = m.formError(r, err, "display_name")
	} else {
		v.Profile = notice{Text: "Your display name was saved."}
	}

	m.section(w, r, status, v, profileSection)
}

func (m *Module) emailAction(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	if !m.parseForm(w, r) {
		return
	}

	email := r.PostForm.Get("email")
	res, err := m.profile.ChangeEmail(r.Context(), m.Actor(r.Context()), email, r.PostForm.Get("current_password"))

	v, verr := m.accountView(r)
	if verr != nil {
		m.pageFailed(w, r, verr)

		return
	}

	status := http.StatusOK

	switch {
	case err != nil:
		v.Email, status = m.formError(r, err, "email")
	case res.Pending:
		v.Email = notice{Text: "We sent a confirmation link to " + email + ". The new address applies once you open it."}
	case email == "":
		v.Email = notice{Text: "Your e-mail address was removed."}
	default:
		v.Email = notice{Text: "Your e-mail address was saved."}
	}

	m.section(w, r, status, v, emailSection)
}

func (m *Module) revokeSessionAction(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	ref := chi.URLParam(r, "ref")
	st := FromContext(r.Context())

	err := m.accounts.RevokeOwnSession(r.Context(), m.Actor(r.Context()), ref)
	if err == nil && st.session != nil && st.session.Ref() == ref {
		// The current session: this is a sign out.
		http.SetCookie(w, m.cookie(m.sessionCookieName(), "", -1))
		m.redirect(w, r, "/login")

		return
	}

	m.sessionsResult(w, r, err, "The session was signed out.")
}

func (m *Module) revokeOthersAction(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	n, err := m.accounts.RevokeOtherSessions(r.Context(), m.Actor(r.Context()))
	m.sessionsResult(w, r, err, fmt.Sprintf("%d other session(s) signed out.", n))
}

func (m *Module) sessionsResult(w http.ResponseWriter, r *http.Request, err error, ok string) {
	v, verr := m.accountView(r)
	if verr != nil {
		m.pageFailed(w, r, verr)

		return
	}

	status := http.StatusOK

	switch {
	case errors.Is(err, domain.ErrSessionNotFound):
		v.SessionsMsg, status = notice{Text: "That session is already signed out.", Error: true}, http.StatusNotFound
	case err != nil:
		v.SessionsMsg, status = m.formError(r, err, "")
	default:
		v.SessionsMsg = notice{Text: ok}
	}

	m.section(w, r, status, v, sessionsSection)
}

// parseForm parses a form body; it answers the error itself.
func (m *Module) parseForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest

		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}

		m.pages.Error(w, r, status)

		return false
	}

	return true
}

// E-mail confirmation (anyone holding the link; GET shows, POST applies).

const pageTitleEmail = "Confirm your e-mail address"

func (m *Module) emailVerifyPage(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)

	token := chi.URLParam(r, "token")
	if err := m.profile.CheckEmailToken(r.Context(), token); err != nil {
		m.pages.Page(w, r, http.StatusNotFound, pageTitleEmail, emailVerifyResult(false, "This link has expired or was already used."), nil)

		return
	}

	m.pages.Page(w, r, http.StatusOK, pageTitleEmail, emailVerifyPage(token), nil)
}

func (m *Module) emailVerifyLanding(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)
	m.pages.Page(w, r, http.StatusNotFound, pageTitleEmail, emailVerifyResult(false, "Open the link from the e-mail again to confirm your address."), nil)
}

func (m *Module) emailVerifyAction(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)

	if !m.parseForm(w, r) {
		return
	}

	err := m.profile.ConfirmEmail(r.Context(), r.PostForm.Get("token"), m.meta(r.Context()))

	switch {
	case err == nil:
		m.pages.Page(w, r, http.StatusOK, pageTitleEmail, emailVerifyResult(true, "Your e-mail address is confirmed."), nil)
	case errors.Is(err, domain.ErrInvalidToken):
		m.pages.Page(w, r, http.StatusNotFound, pageTitleEmail, emailVerifyResult(false, "This link has expired or was already used."), nil)
	default:
		m.logger.ErrorContext(r.Context(), "e-mail confirmation failed", slog.Any("error", err))
		m.pages.Error(w, r, http.StatusInternalServerError)
	}
}
