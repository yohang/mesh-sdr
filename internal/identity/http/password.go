package http

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// Password form messages.
const (
	msgPasswordMismatch  = "The new passwords do not match."
	msgCurrentIncorrect  = "The current password is incorrect."
	msgPasswordFailed    = "The password could not be changed. Try again later."
	msgPasswordChanged   = "Your password was changed. Your other sessions were signed out."
	passwordChangedParam = "changed"
)

// passwordForm is the view of the password change form.
type passwordForm struct {
	// Forced tells that the user must change the password (AUTH-006).
	Forced    bool
	Changed   bool
	Next      string
	MinLength int
	Error     string
	// Field is the field the error is about: current or new.
	Field string
}

const pageTitlePassword = "Change password"

func (m *Module) passwordPage(w http.ResponseWriter, r *http.Request) {
	f := passwordForm{
		Forced:    m.Principal(r.Context()).MustChangePassword(),
		Changed:   r.URL.Query().Get(passwordChangedParam) == "1",
		Next:      SafeNext(r.URL.Query().Get("next"), m.routes),
		MinLength: m.passwords.MinLength(r.Context()),
	}

	m.page(w, r, http.StatusOK, pageTitlePassword, passwordPage(f), nil)
}

func (m *Module) passwordAction(w http.ResponseWriter, r *http.Request) {
	if !m.pages.ParseForm(w, r, 0) {
		return
	}

	f := passwordForm{
		Forced:    m.Principal(r.Context()).MustChangePassword(),
		Next:      SafeNext(r.PostForm.Get("next"), m.routes),
		MinLength: m.passwords.MinLength(r.Context()),
	}

	newPassword := r.PostForm.Get("new_password")
	if newPassword != r.PostForm.Get("confirm_password") {
		f.Error, f.Field = msgPasswordMismatch, "new"
		m.page(w, r, http.StatusUnprocessableEntity, pageTitlePassword, passwordPage(f), passwordFormView(f))

		return
	}

	_, _, cookie, forced, err := m.ChangePassword(r.Context(), r.PostForm.Get("current_password"), newPassword)
	if err == nil {
		http.SetCookie(w, cookie)

		// Full page load: the next page fetches the new session's CSRF
		// token.
		if forced {
			render.Redirect(w, r, f.Next)
		} else {
			render.Redirect(w, r, PasswordChangePath+"?"+passwordChangedParam+"=1")
		}

		return
	}

	status := http.StatusUnprocessableEntity

	var (
		rl *domain.RateLimitError
		de *shared.Error
	)

	switch {
	case errors.As(err, &rl):
		status = http.StatusTooManyRequests
		f.Error, f.Field = fmt.Sprintf(msgThrottled, humanWait(rl.RetryAfter())), "current"
		w.Header().Set("Retry-After", fmt.Sprint(int(rl.RetryAfter().Seconds())))
	case errors.Is(err, domain.ErrInvalidCurrentPassword):
		f.Error, f.Field = msgCurrentIncorrect, "current"
	case errors.Is(err, domain.ErrInvalidPassword) && errors.As(err, &de):
		f.Error, f.Field = render.Sentence(de.Message()), "new"
	default:
		status = http.StatusInternalServerError
		f.Error = msgPasswordFailed

		m.logger.ErrorContext(r.Context(), "password change failed", slog.Any("error", err))
	}

	m.page(w, r, status, pageTitlePassword, passwordPage(f), passwordFormView(f))
}
