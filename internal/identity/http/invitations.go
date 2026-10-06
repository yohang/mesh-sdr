package http

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// InvitationsPath is the admin page of invitations (ACC-002).
const InvitationsPath = "/admin/invitations"

const (
	pageTitleInvitations = "Invitations"
	pageTitleInvite      = "Create your account"
	msgInviteInvalid     = "This invitation is no longer valid. Ask the administrator for a new one."
)

// invitationsView is the admin page of invitations.
type invitationsView struct {
	Now         time.Time
	List        []*domain.Invitation
	MailEnabled bool
	DefaultDays int
	Created     *app.CreatedInvitation
	Form        inviteForm
	Notice      notice
}

// inviteForm is the creation form as typed.
type inviteForm struct {
	Role   string
	Device string
	Email  string
	Days   string
}

func (m *Module) invitationsView(r *http.Request) (invitationsView, error) {
	ctx := r.Context()

	list, err := m.invitations.List(ctx)
	if err != nil {
		return invitationsView{}, err
	}

	return invitationsView{
		Now: m.invitations.Now(), List: list, MailEnabled: m.invitations.MailEnabled(),
		DefaultDays: int(m.invitations.DefaultTTL(ctx) / (24 * time.Hour)), Form: inviteForm{Role: "listener"},
	}, nil
}

func (m *Module) invitationsPage(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	v, err := m.invitationsView(r)
	if err != nil {
		m.pageFailed(w, r, err)

		return
	}

	m.pages.Page(w, r, http.StatusOK, pageTitleInvitations, invitationsPage(v), nil)
}

func (m *Module) createInvitationAction(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	if !m.parseForm(w, r) {
		return
	}

	f := inviteForm{Role: r.PostForm.Get("role"), Device: r.PostForm.Get("device_id"), Email: r.PostForm.Get("email"), Days: r.PostForm.Get("days")}

	var (
		created app.CreatedInvitation
		err     error
	)

	role, rerr := domain.ParseRole(f.Role)

	days, derr := strconv.Atoi(f.Days)
	if f.Days == "" {
		days, derr = 0, nil
	}

	switch {
	case rerr != nil || role == domain.RoleAnonymous:
		err = domain.ErrInvalidRole
	case derr != nil || days < 0 || days > 30:
		err = domain.ErrInvalidInvitation.WithDetail("an invitation expires after 1 to 30 days")
	default:
		created, err = m.invitations.Create(r.Context(), m.Actor(r.Context()), app.CreateInvitationInput{
			Role: role, Device: f.Device, Email: f.Email, TTL: time.Duration(days) * 24 * time.Hour,
		})
	}

	v, verr := m.invitationsView(r)
	if verr != nil {
		m.pageFailed(w, r, verr)

		return
	}

	status := http.StatusOK

	if err != nil {
		v.Form = f
		v.Notice, status = m.formError(r, err, "")
	} else {
		v.Created = &created
	}

	m.pages.Page(w, r, status, pageTitleInvitations, invitationsPage(v), nil)
}

func (m *Module) revokeInvitationAction(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	var err error

	id, perr := domain.ParseInvitationID(chi.URLParam(r, "id"))
	if perr != nil {
		err = domain.ErrInvitationNotFound
	} else {
		err = m.invitations.Revoke(r.Context(), m.Actor(r.Context()), id)
	}

	v, verr := m.invitationsView(r)
	if verr != nil {
		m.pageFailed(w, r, verr)

		return
	}

	status := http.StatusOK
	if err != nil {
		v.Notice, status = m.formError(r, err, "")
	} else {
		v.Notice = notice{Text: "The invitation was revoked."}
	}

	m.pages.Page(w, r, status, pageTitleInvitations, invitationsPage(v), nil)
}

func (m *Module) testMailAction(w http.ResponseWriter, r *http.Request) {
	noIndex(w)

	to, err := m.invitations.TestMail(r.Context(), m.Actor(r.Context()))

	v, verr := m.invitationsView(r)
	if verr != nil {
		m.pageFailed(w, r, verr)

		return
	}

	status := http.StatusOK

	switch {
	case errors.Is(err, app.ErrMailDisabled):
		v.Notice, status = notice{Text: "Mail is not configured.", Error: true}, http.StatusConflict
	case err != nil:
		v.Notice, status = m.formError(r, err, "")
	default:
		v.Notice = notice{Text: "A test message was queued for " + to.String() + "."}
	}

	m.pages.Page(w, r, status, pageTitleInvitations, invitationsPage(v), nil)
}

// Invitation acceptance (anyone holding the link).

// acceptForm is the acceptance form.
type acceptForm struct {
	Token       string
	Role        string
	Email       string // fixed by the invitation
	Username    string
	DisplayName string
	OwnEmail    string
	MinLength   int
	Error       string
	Field       string
}

func (m *Module) invitePage(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)

	if m.Principal(r.Context()).UserID().IsZero() {
		token := chi.URLParam(r, "token")

		inv, err := m.invitations.Check(r.Context(), token, m.meta(r.Context()))
		if err != nil {
			m.inviteRefused(w, r, err)

			return
		}

		f := acceptForm{Token: token, Role: inv.Role().String(), Email: inv.Email().String(), MinLength: m.invitations.MinLength(r.Context())}
		m.pages.Page(w, r, http.StatusOK, pageTitleInvite, invitePage(f), nil)

		return
	}

	m.pages.Page(w, r, http.StatusConflict, pageTitleInvite, inviteMessage("You are signed in. Sign out, then open the invitation link again."), nil)
}

func (m *Module) inviteLanding(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)
	m.pages.Page(w, r, http.StatusNotFound, pageTitleInvite, inviteMessage("Open the invitation link again to create your account."), nil)
}

// inviteRefused shows why an invitation link cannot be used: invalid (404,
// one message for every reason) or too many attempts (429).
func (m *Module) inviteRefused(w http.ResponseWriter, r *http.Request, err error) {
	var rl *domain.RateLimitError

	switch {
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", fmt.Sprint(int(rl.RetryAfter().Seconds())))
		m.pages.Page(w, r, http.StatusTooManyRequests, pageTitleInvite, inviteMessage(fmt.Sprintf(msgThrottled, humanWait(rl.RetryAfter()))), nil)
	case errors.Is(err, domain.ErrInvitationInvalid):
		m.pages.Page(w, r, http.StatusNotFound, pageTitleInvite, inviteMessage(msgInviteInvalid), nil)
	default:
		m.logger.ErrorContext(r.Context(), "invitation check failed", slog.Any("error", err))
		m.pages.Error(w, r, http.StatusInternalServerError)
	}
}

func (m *Module) acceptAction(w http.ResponseWriter, r *http.Request) {
	setupHeaders(w)

	if !m.parseForm(w, r) {
		return
	}

	if !m.Principal(r.Context()).IsAnonymous() {
		m.pages.Page(w, r, http.StatusConflict, pageTitleInvite, inviteMessage("You are signed in. Sign out, then open the invitation link again."), nil)

		return
	}

	f := acceptForm{
		Token: r.PostForm.Get("token"), Role: r.PostForm.Get("role"), Email: r.PostForm.Get("invited_email"),
		Username: r.PostForm.Get("username"), DisplayName: r.PostForm.Get("display_name"), OwnEmail: r.PostForm.Get("email"),
		MinLength: m.invitations.MinLength(r.Context()),
	}

	password := r.PostForm.Get("password")
	if password != r.PostForm.Get("confirm_password") {
		f.Error, f.Field = "The passwords do not match.", "password"
		m.pages.Page(w, r, http.StatusUnprocessableEntity, pageTitleInvite, invitePage(f), acceptFormView(f))

		return
	}

	res, err := m.invitations.Accept(r.Context(), app.AcceptInput{
		Token: f.Token, Username: f.Username, DisplayName: f.DisplayName, Email: f.OwnEmail, Password: password,
		Meta: m.meta(r.Context()),
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
	case errors.Is(err, domain.ErrInvitationInvalid), errors.Is(err, domain.ErrRateLimited):
		m.inviteRefused(w, r, err)

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

		m.logger.ErrorContext(r.Context(), "invitation acceptance failed", slog.Any("error", err))
	}

	m.pages.Page(w, r, status, pageTitleInvite, invitePage(f), acceptFormView(f))
}
