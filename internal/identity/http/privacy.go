package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// writeExport sends an account export as a JSON download.
func (m *Module) writeExport(w http.ResponseWriter, r *http.Request, e app.Export, err error) {
	if errors.Is(err, domain.ErrUserNotFound) {
		m.pages.Error(w, r, http.StatusNotFound)

		return
	}

	if err != nil {
		m.pageFailed(w, r, err)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="account-`+e.Account.Username+`.json"`)
	w.Header().Set("Cache-Control", "no-store")

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(e)
}

func (m *Module) exportOwnAction(w http.ResponseWriter, r *http.Request) {
	e, err := m.accounts.ExportOwn(r.Context(), m.Actor(r.Context()))
	m.writeExport(w, r, e, err)
}

func (m *Module) userExportAction(w http.ResponseWriter, r *http.Request) {
	id, err := domain.ParseUserID(chi.URLParam(r, "id"))
	if err != nil {
		m.pages.Error(w, r, http.StatusNotFound)

		return
	}

	e, err := m.accounts.ExportUser(r.Context(), m.Actor(r.Context()), id)
	m.writeExport(w, r, e, err)
}

func (m *Module) deleteOwnAction(w http.ResponseWriter, r *http.Request) {
	if !m.pages.ParseForm(w, r, 0) {
		return
	}

	var err error
	if r.PostForm.Get("confirm") != "1" {
		err = domain.ErrInvalidUser.WithDetail("tick the box to confirm that the account is deleted for good")
	} else {
		err = m.accounts.DeleteOwn(r.Context(), m.Actor(r.Context()), r.PostForm.Get("current_password"))
	}

	if err == nil {
		http.SetCookie(w, m.cookie(m.sessionCookieName(), "", -1))
		render.Redirect(w, r, "/")

		return
	}

	v, verr := m.accountView(r)
	if verr != nil {
		m.pageFailed(w, r, verr)

		return
	}

	var status int

	v.Data, status = m.formError(r, err, "")
	m.section(w, r, status, v, dataSection)
}

func (m *Module) userDeleteAction(w http.ResponseWriter, r *http.Request) {
	if !m.pages.ParseForm(w, r, 0) {
		return
	}

	id, err := domain.ParseUserID(chi.URLParam(r, "id"))
	if err != nil {
		m.pages.Error(w, r, http.StatusNotFound)

		return
	}

	if r.PostForm.Get("confirm") == "1" {
		if err = m.accounts.Delete(r.Context(), m.Actor(r.Context()), id); err == nil {
			if m.Principal(r.Context()).UserID() == id {
				http.SetCookie(w, m.cookie(m.sessionCookieName(), "", -1))
				render.Redirect(w, r, "/")

				return
			}

			render.Redirect(w, r, UsersPath)

			return
		}
	} else {
		err = domain.ErrInvalidUser.WithDetail("tick the box to confirm that the account is deleted for good")
	}

	m.userAction(w, r, func(domain.UserID, *userView) (string, error) { return "", err })
}
