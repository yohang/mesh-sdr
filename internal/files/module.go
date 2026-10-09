package files

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

//go:generate go tool templ generate

// SitePath is the admin Site page that shows the images section.
const SitePath = "/admin/site"

// Module serves the images section actions of the admin Site page.
type Module struct {
	branding *Branding
	render   *render.Renderer
	guard    func(http.Handler) http.Handler
	user     func(ctx context.Context) shared.UUID
	logger   *slog.Logger
}

// New returns the module. guard checks the admin role and network; user
// returns the signed-in user of a request.
func New(branding *Branding, rd *render.Renderer, guard func(http.Handler) http.Handler, user func(ctx context.Context) shared.UUID,
	logger *slog.Logger,
) *Module {
	return &Module{branding: branding, render: rd, guard: guard, user: user, logger: logger}
}

// Middlewares implements internal/http.Module.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module.
func (m *Module) Routes(r chi.Router) {
	r.With(m.guard).Post(SitePath+"/images", m.upload)
	r.With(m.guard).Post(SitePath+"/images/remove", m.remove)
}

// Outcomes of an image action, shown on the Site page after the redirect
// of a form sent without JavaScript (?done=…&slot=…).
const (
	doneUpdated   = "image_updated"
	doneRemoved   = "image_removed"
	doneUnchanged = "image_unchanged"
)

// notice is the text of an outcome.
func notice(done string, slot Slot) string {
	switch done {
	case doneUpdated:
		return "The " + slot.Name() + " was updated."
	case doneRemoved:
		return "The " + slot.Name() + " was removed: the default applies."
	case doneUnchanged:
		return "The " + slot.Name() + " was already the default."
	default:
		return ""
	}
}

// Section implements the Site page's images section, with the outcome of
// the action that redirected to it.
func (m *Module) Section(r *http.Request) templ.Component {
	q := r.URL.Query()

	text := ""
	if slot, err := ParseSlot(q.Get("slot")); err == nil {
		text = notice(q.Get("done"), slot)
	}

	return imagesSection(m.views(r.Context()), text, "")
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

// done answers a successful action: the section (htmx), or the Site page
// showing the outcome.
func (m *Module) done(w http.ResponseWriter, r *http.Request, done string, slot Slot) {
	if !render.WantsFragment(r) {
		render.Redirect(w, r, SitePath+"?"+url.Values{"done": {done}, "slot": {slot.Name()}}.Encode()+"#receiver-images")

		return
	}

	m.respond(w, r, http.StatusOK, notice(done, slot), "")
}

// respond answers the section with an outcome: alone for htmx, in an
// admin page otherwise.
func (m *Module) respond(w http.ResponseWriter, r *http.Request, status int, text, failure string) {
	section := imagesSection(m.views(r.Context()), text, failure)
	m.render.AdminPage(w, r, status, "Site", "site", imagesPage(section), section)
}

func (m *Module) upload(w http.ResponseWriter, r *http.Request) {
	if err := CheckDeclaredSize(r, SlotPanorama); err != nil {
		m.respond(w, r, http.StatusRequestEntityTooLarge, "", "The image is too large: the avatar must not exceed 250 KiB and the panorama 2 MiB.")

		return
	}

	LimitBody(w, r, SlotPanorama)

	mr, err := r.MultipartReader()
	if err != nil {
		m.render.Error(w, r, http.StatusBadRequest)

		return
	}

	slot, data, err := ReadUpload(mr, func(fields map[string]string) (Slot, error) { return ParseSlot(fields["slot"]) })

	switch {
	case errors.Is(err, ErrTooLarge):
		m.respond(w, r, http.StatusRequestEntityTooLarge, "", "The "+slot.Name()+" is too large.")

		return
	case errors.Is(err, ErrUnknownImageSlot):
		m.render.Error(w, r, http.StatusNotFound)

		return
	case err != nil:
		m.respond(w, r, http.StatusBadRequest, "", "Choose an image file to upload.")

		return
	}

	if _, err := m.branding.Upload(r.Context(), m.user(r.Context()), slot, data); err != nil {
		var de *shared.Error
		if errors.As(err, &de) && de.Kind() == shared.KindInvalid {
			m.respond(w, r, http.StatusUnprocessableEntity, "", render.Sentence(de.Message()))

			return
		}

		m.logger.ErrorContext(r.Context(), "upload receiver image", slog.String("slot", slot.Name()), slog.Any("error", err))
		m.respond(w, r, http.StatusInternalServerError, "", "The image could not be saved. Try again later.")

		return
	}

	m.done(w, r, doneUpdated, slot)
}

func (m *Module) remove(w http.ResponseWriter, r *http.Request) {
	if !m.render.ParseForm(w, r, 4<<10) {
		return
	}

	slot, err := ParseSlot(r.PostForm.Get("slot"))
	if err != nil {
		m.render.Error(w, r, http.StatusNotFound)

		return
	}

	removed, err := m.branding.Remove(r.Context(), slot)
	if err != nil {
		m.logger.ErrorContext(r.Context(), "remove receiver image", slog.String("slot", slot.Name()), slog.Any("error", err))
		m.respond(w, r, http.StatusInternalServerError, "", "The image could not be removed. Try again later.")

		return
	}

	if removed {
		m.done(w, r, doneRemoved, slot)
	} else {
		m.done(w, r, doneUnchanged, slot)
	}
}
