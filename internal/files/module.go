package files

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

//go:generate go tool templ generate

// Module serves the images section actions of the admin Site page.
type Module struct {
	branding *Branding
	guard    func(http.Handler) http.Handler
	user     func(ctx context.Context) shared.UUID
	errorPg  func(w http.ResponseWriter, r *http.Request, status int)
	logger   *slog.Logger
}

// New returns the module. guard checks the admin role and network; user
// returns the signed-in user of a request; errorPage writes the shell error
// page.
func New(branding *Branding, guard func(http.Handler) http.Handler, user func(ctx context.Context) shared.UUID,
	errorPage func(w http.ResponseWriter, r *http.Request, status int), logger *slog.Logger,
) *Module {
	return &Module{branding: branding, guard: guard, user: user, errorPg: errorPage, logger: logger}
}

// Middlewares implements internal/http.Module.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module.
func (m *Module) Routes(r chi.Router) {
	r.With(m.guard).Post("/admin/site/images", m.upload)
	r.With(m.guard).Post("/admin/site/images/remove", m.remove)
}

// Section implements the Site page's images section.
func (m *Module) Section(r *http.Request) templ.Component {
	return imagesSection(m.views(r.Context()), "", "")
}

type slotView struct {
	Slot     Slot
	Label    string
	Limit    string
	Set      bool
	Src      string
	Width    int
	Height   int
	FileName string
}

func (m *Module) views(ctx context.Context) []slotView {
	var out []slotView

	for _, s := range Slots() {
		v := slotView{Slot: s, Label: "Avatar", Limit: "250 KiB"}
		if s == SlotPanorama {
			v.Label, v.Limit = "Panorama", "2 MiB"
		}

		f, err := m.branding.Current(ctx, s)

		switch {
		case err == nil:
			sum := f.SHA256()
			v.Set, v.Width, v.Height, v.FileName = true, f.Width(), f.Height(), f.Name()
			v.Src = "/api/v1/branding/" + s.Name() + "?v=" + hex.EncodeToString(sum[:6])
		case !errors.Is(err, ErrImageNotSet):
			m.logger.WarnContext(ctx, "receiver image unavailable", slog.String("slot", s.Name()), slog.Any("error", err))
		}

		out = append(out, v)
	}

	return out
}

// respond returns the section (htmx) or goes back to the Site page.
func (m *Module) respond(w http.ResponseWriter, r *http.Request, status int, notice, failure string) {
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/admin/site", http.StatusSeeOther)

		return
	}

	w.Header().Add("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if err := imagesSection(m.views(r.Context()), notice, failure).Render(r.Context(), w); err != nil {
		m.logger.ErrorContext(r.Context(), "render images section", slog.Any("error", err))
	}
}

func (m *Module) upload(w http.ResponseWriter, r *http.Request) {
	if err := CheckDeclaredSize(r, SlotPanorama); err != nil {
		m.respond(w, r, http.StatusRequestEntityTooLarge, "", "The image is too large: the avatar must not exceed 250 KiB and the panorama 2 MiB.")

		return
	}

	LimitBody(w, r, SlotPanorama)

	mr, err := r.MultipartReader()
	if err != nil {
		m.errorPg(w, r, http.StatusBadRequest)

		return
	}

	slot, data, err := ReadUpload(mr, func(fields map[string]string) (Slot, error) { return ParseSlot(fields["slot"]) })

	switch {
	case errors.Is(err, ErrTooLarge):
		m.respond(w, r, http.StatusRequestEntityTooLarge, "", "The "+slot.Name()+" is too large.")

		return
	case errors.Is(err, ErrUnknownImageSlot):
		m.errorPg(w, r, http.StatusNotFound)

		return
	case err != nil:
		m.respond(w, r, http.StatusBadRequest, "", "Choose an image file to upload.")

		return
	}

	if _, err := m.branding.Upload(r.Context(), m.user(r.Context()), slot, data); err != nil {
		var de *shared.Error
		if errors.As(err, &de) && de.Kind() == shared.KindInvalid {
			m.respond(w, r, http.StatusUnprocessableEntity, "", de.Message()+".")

			return
		}

		m.logger.ErrorContext(r.Context(), "upload receiver image", slog.String("slot", slot.Name()), slog.Any("error", err))
		m.respond(w, r, http.StatusInternalServerError, "", "The image could not be saved. Try again later.")

		return
	}

	m.respond(w, r, http.StatusOK, "The "+slot.Name()+" was updated.", "")
}

func (m *Module) remove(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		m.errorPg(w, r, http.StatusBadRequest)

		return
	}

	slot, err := ParseSlot(r.PostForm.Get("slot"))
	if err != nil {
		m.errorPg(w, r, http.StatusNotFound)

		return
	}

	removed, err := m.branding.Remove(r.Context(), slot)
	if err != nil {
		m.logger.ErrorContext(r.Context(), "remove receiver image", slog.String("slot", slot.Name()), slog.Any("error", err))
		m.respond(w, r, http.StatusInternalServerError, "", "The image could not be removed. Try again later.")

		return
	}

	notice := "The " + slot.Name() + " was already the default."
	if removed {
		notice = "The " + slot.Name() + " was removed: the default applies."
	}

	m.respond(w, r, http.StatusOK, notice, "")
}
