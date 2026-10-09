package http

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// Password reset pages (ACC-003, FEATURE_SPEC §10.3).
const (
	ForgotPath = "/password/forgot"
	ResetPath  = "/password/reset"

	pageTitleForgot = "Forgot password"
	pageTitleReset  = "Reset password"
	msgResetInvalid = "This link has expired or was already used."
)

// forgotView is the forgot-password page.
type forgotView struct {
	MailEnabled bool
	Sent        bool
	Minutes     int
	Error       string
}

func (m *Module) forgotView(r *http.Request) forgotView {
	return forgotView{MailEnabled: m.resets.MailEnabled(), Minutes: int(m.resets.TTL(r.Context()) / time.Minute)}
}

func (m *Module) forgotPage(w http.ResponseWriter, r *http.Request) {
	m.page(w, r, http.StatusOK, pageTitleForgot, forgotPage(m.forgotView(r)), nil)
}

func (m *Module) forgotAction(w http.ResponseWriter, r *http.Request) {
	if !m.pages.ParseForm(w, r, 0) {
		return
	}

	v := m.forgotView(r)
	status := http.StatusOK

	err := m.resets.Request(r.Context(), r.PostForm.Get("login"), m.meta(r.Context()))

	var rl *domain.RateLimitError

	switch {
	case errors.As(err, &rl):
		status = http.StatusTooManyRequests
		v.Error = fmt.Sprintf(msgThrottled, humanWait(rl.RetryAfter()))
		w.Header().Set("Retry-After", fmt.Sprint(int(rl.RetryAfter().Seconds())))
	case err != nil:
		status = http.StatusInternalServerError
		v.Error = msgSaveFailed

		m.logger.ErrorContext(r.Context(), "password reset request failed", slog.Any("error", err))
	default:
		v.Sent = true
	}

	m.page(w, r, status, pageTitleForgot, forgotPage(v), forgotFormView(v))
}

// resetForm is the reset page's form.
type resetForm struct {
	Token     string
	MinLength int
	Error     string
}

func (m *Module) resetPage(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)

	token := chi.URLParam(r, "token")
	if err := m.resets.Check(r.Context(), token, m.meta(r.Context())); err != nil {
		m.resetRefused(w, r, err)

		return
	}

	m.page(w, r, http.StatusOK, pageTitleReset, resetPage(resetForm{Token: token, MinLength: m.resets.MinLength(r.Context())}), nil)
}

func (m *Module) resetLanding(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)
	m.resetRefused(w, r, domain.ErrInvalidToken)
}

func (m *Module) resetRefused(w http.ResponseWriter, r *http.Request, err error) {
	var rl *domain.RateLimitError

	switch {
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", fmt.Sprint(int(rl.RetryAfter().Seconds())))
		m.page(w, r, http.StatusTooManyRequests, pageTitleReset, resetInvalid(fmt.Sprintf(msgThrottled, humanWait(rl.RetryAfter()))), nil)
	case errors.Is(err, domain.ErrInvalidToken):
		m.page(w, r, http.StatusNotFound, pageTitleReset, resetInvalid(msgResetInvalid), nil)
	default:
		m.logger.ErrorContext(r.Context(), "password reset check failed", slog.Any("error", err))
		m.pages.Error(w, r, http.StatusInternalServerError)
	}
}

func (m *Module) resetAction(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)

	if !m.pages.ParseForm(w, r, 0) {
		return
	}

	f := resetForm{Token: r.PostForm.Get("token"), MinLength: m.resets.MinLength(r.Context())}

	password := r.PostForm.Get("password")
	if password != r.PostForm.Get("confirm_password") {
		f.Error = "The passwords do not match."
		m.page(w, r, http.StatusUnprocessableEntity, pageTitleReset, resetPage(f), resetFormView(f))

		return
	}

	err := m.resets.Confirm(r.Context(), f.Token, password, m.meta(r.Context()))

	var de *shared.Error

	switch {
	case err == nil:
		// Every session of the user is revoked, the request's included.
		http.SetCookie(w, m.cookie(m.sessionCookieName(), "", -1))
		render.Redirect(w, r, "/login?reset=1")
	case errors.Is(err, domain.ErrInvalidToken), errors.Is(err, domain.ErrRateLimited):
		m.resetRefused(w, r, err)
	case errors.Is(err, domain.ErrInvalidPassword) && errors.As(err, &de):
		f.Error = render.Sentence(de.Message())
		m.page(w, r, http.StatusUnprocessableEntity, pageTitleReset, resetPage(f), resetFormView(f))
	default:
		m.logger.ErrorContext(r.Context(), "password reset failed", slog.Any("error", err))
		m.pages.Error(w, r, http.StatusInternalServerError)
	}
}
