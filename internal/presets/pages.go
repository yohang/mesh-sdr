package presets

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

//go:generate go tool templ generate

// Renderer renders pages in the app shell (internal/web/render).
type Renderer interface {
	Page(w http.ResponseWriter, r *http.Request, status int, page layout.Page, content, fragment templ.Component)
	Error(w http.ResponseWriter, r *http.Request, status int)
}

// PagesDeps are the dependencies of the admin pages.
type PagesDeps struct {
	Render  Renderer
	Guard   func(http.Handler) http.Handler // admin role and network (identity)
	Service *Service
	Logger  *slog.Logger
}

// Pages serves Admin › Presets (ADM-017): list, create, edit and delete
// as HTML forms (ADR 0023, ADR 0026). It is an internal/http.Module.
type Pages struct{ d PagesDeps }

// NewPages returns the module.
func NewPages(d PagesDeps) *Pages { return &Pages{d: d} }

// Middlewares implements internal/http.Module.
func (m *Pages) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module.
func (m *Pages) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(m.d.Guard, noIndex)

		for path, h := range map[string]http.HandlerFunc{
			"/admin/presets": m.list, "/admin/presets/new": m.newPage, "/admin/presets/{id}": m.editPage,
			"/admin/presets/{id}/delete": m.deletePage,
		} {
			r.Get(path, h)
			r.Head(path, h)
		}

		r.Post("/admin/presets", m.create)
		r.Post("/admin/presets/{id}", m.replace)
		r.Post("/admin/presets/{id}/delete", m.delete)
		r.Post("/admin/presets/{id}/clone", m.clone)
		r.Post("/admin/presets/{id}/move", m.move)
	})
}

func noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex")
		next.ServeHTTP(w, r)
	})
}

func (m *Pages) page(w http.ResponseWriter, r *http.Request, status int, title string, content templ.Component) {
	m.pageWith(w, r, status, title, content, nil)
}

func (m *Pages) pageWith(w http.ResponseWriter, r *http.Request, status int, title string, content, fragment templ.Component) {
	m.d.Render.Page(w, r, status, layout.Page{Title: title, Section: layout.SectionAdmin}, layout.AdminPage("presets", content), fragment)
}

// Notices shown after a redirect (?done=…).
var notices = map[string]string{
	"created": "Preset created.",
	"saved":   "Preset saved.",
	"deleted": "Preset deleted.",
	"cloned":  "Preset cloned: this is the copy.",
}

func (m *Pages) list(w http.ResponseWriter, r *http.Request) {
	list, err := m.d.Service.List(r.Context())
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "list presets", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	m.page(w, r, http.StatusOK, "Presets", listPage(list, notices[r.URL.Query().Get("done")]))
}

func movePath(p *Preset) string { return "/admin/presets/" + p.ID().String() + "/move" }

// clone stores a copy of a preset and opens it (ADM-019).
func (m *Pages) clone(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r, m.d.Render) {
		return
	}

	p, err := m.d.Service.Clone(r.Context(), chi.URLParam(r, "id"))

	switch {
	case err == nil:
		redirect(w, r, "/admin/presets/"+p.ID().String()+"?done=cloned")
	case errors.Is(err, ErrPresetNotFound):
		m.d.Render.Error(w, r, http.StatusNotFound)
	default:
		m.d.Logger.ErrorContext(r.Context(), "clone preset", slog.String("preset_id", chi.URLParam(r, "id")), slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)
	}
}

// move changes the position of a preset (ADM-021): direction=up|down, or
// position=<1-based position> (drag and drop). It answers the list. It
// never retunes a device.
func (m *Pages) move(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r, m.d.Render) {
		return
	}

	var (
		moved bool
		err   error
		id    = chi.URLParam(r, "id")
	)

	switch dir, pos := r.PostForm.Get("direction"), r.PostForm.Get("position"); {
	case dir == "up":
		moved, err = m.d.Service.MoveBy(r.Context(), id, -1)
	case dir == "down":
		moved, err = m.d.Service.MoveBy(r.Context(), id, 1)
	case pos != "":
		n, perr := strconv.Atoi(pos)
		if perr != nil || n < 1 {
			m.d.Render.Error(w, r, http.StatusBadRequest)

			return
		}

		moved, err = m.d.Service.MoveTo(r.Context(), id, n-1)
	default:
		m.d.Render.Error(w, r, http.StatusBadRequest)

		return
	}

	switch {
	case errors.Is(err, ErrPresetNotFound):
		m.d.Render.Error(w, r, http.StatusNotFound)

		return
	case err != nil:
		m.d.Logger.ErrorContext(r.Context(), "move preset", slog.String("preset_id", id), slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	list, err := m.d.Service.List(r.Context())
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "list presets", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	notice := ""
	if moved {
		notice = "Preset moved."
	}

	m.pageWith(w, r, http.StatusOK, "Presets", listPage(list, notice), presetsList(list, notice))
}

func (m *Pages) newPage(w http.ResponseWriter, r *http.Request) {
	m.page(w, r, http.StatusOK, "New preset", formPage(presetForm{Values: map[string]string{"start_mod": DefaultStartMod}}))
}

// preset returns the preset of the path, or answers 404 (or 500).
func (m *Pages) preset(w http.ResponseWriter, r *http.Request) (*Preset, bool) {
	p, err := m.d.Service.Get(r.Context(), chi.URLParam(r, "id"))

	switch {
	case errors.Is(err, ErrPresetNotFound):
		m.d.Render.Error(w, r, http.StatusNotFound)

		return nil, false
	case err != nil:
		m.d.Logger.ErrorContext(r.Context(), "read preset", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return nil, false
	}

	return p, true
}

func (m *Pages) editPage(w http.ResponseWriter, r *http.Request) {
	p, ok := m.preset(w, r)
	if !ok {
		return
	}

	m.page(w, r, http.StatusOK, "Preset "+p.Name(), formPage(formOf(p, notices[r.URL.Query().Get("done")])))
}

func (m *Pages) deletePage(w http.ResponseWriter, r *http.Request) {
	p, ok := m.preset(w, r)
	if !ok {
		return
	}

	m.page(w, r, http.StatusOK, "Delete "+p.Name(), deletePage(p, ""))
}

func (m *Pages) create(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r, m.d.Render) {
		return
	}

	f := presetForm{Values: formValues(r)}

	d, ok := draftOf(&f)
	if ok {
		p, err := m.d.Service.Create(r.Context(), d)
		if err == nil {
			redirect(w, r, "/admin/presets/"+p.ID().String()+"?done=created")

			return
		}

		m.formError(r, &f, err)
	}

	m.page(w, r, f.status(), "New preset", formPage(f))
}

func (m *Pages) replace(w http.ResponseWriter, r *http.Request) {
	cur, ok := m.preset(w, r)
	if !ok || !parseForm(w, r, m.d.Render) {
		return
	}

	f := presetForm{ID: cur.ID().String(), Name: cur.Name(), Values: formValues(r)}

	version, err := strconv.Atoi(r.PostForm.Get("version"))
	if err != nil {
		m.d.Render.Error(w, r, http.StatusBadRequest)

		return
	}

	f.Version = version

	d, ok := draftOf(&f)
	if ok {
		// Waterfall levels are not edited here (ADR 0026: one source, the
		// hub setting): a preset keeps the levels it has.
		if lv, set := cur.WaterfallLevels(); set {
			d.WaterfallLevels = &[2]int{lv.Min(), lv.Max()}
		}

		res, err := m.d.Service.Replace(r.Context(), f.ID, version, d)
		if err == nil {
			redirect(w, r, "/admin/presets/"+res.Preset.ID().String()+"?done=saved")

			return
		}

		m.formError(r, &f, err)
	}

	m.page(w, r, f.status(), "Preset "+cur.Name(), formPage(f))
}

func (m *Pages) delete(w http.ResponseWriter, r *http.Request) {
	p, ok := m.preset(w, r)
	if !ok || !parseForm(w, r, m.d.Render) {
		return
	}

	version, verr := strconv.Atoi(r.PostForm.Get("version"))
	if verr != nil {
		m.d.Render.Error(w, r, http.StatusBadRequest)

		return
	}

	err := m.d.Service.Delete(r.Context(), p.ID().String(), version)
	if err == nil {
		redirect(w, r, "/admin/presets?done=deleted")

		return
	}

	status, msg := http.StatusConflict, ""

	var de *shared.Error

	switch {
	case errors.Is(err, ErrPresetInUse) && errors.As(err, &de):
		msg = sentence(de.Message())
	case errors.Is(err, ErrVersionConflict):
		msg = "This preset was changed meanwhile: check it before deleting it."
	case errors.Is(err, ErrPresetNotFound):
		m.d.Render.Error(w, r, http.StatusNotFound)

		return
	default:
		m.d.Logger.ErrorContext(r.Context(), "delete preset", slog.String("preset_id", p.ID().String()), slog.Any("error", err))
		status, msg = http.StatusInternalServerError, "The preset could not be deleted. Try again later."
	}

	m.page(w, r, status, "Delete "+p.Name(), deletePage(p, msg))
}

// formError records a refused save on the form.
func (m *Pages) formError(r *http.Request, f *presetForm, err error) {
	var de *shared.Error

	switch {
	case errors.Is(err, ErrInvalidPreset) && errors.As(err, &de):
		for _, v := range de.Violations() {
			f.setError(v.Path(), sentence(v.Message()))
		}

		if len(f.Errors) == 0 {
			f.Failure = sentence(de.Message())
		}
	case errors.Is(err, ErrSlugTaken):
		f.setError("slug", "Another preset has this slug.")
	case errors.Is(err, ErrVersionConflict):
		f.Failure, f.Conflict = "This preset was changed meanwhile: reload the page to edit the current version.", true
	case errors.Is(err, ErrPresetNotFound):
		f.Failure, f.Conflict = "This preset no longer exists.", true
	default:
		m.d.Logger.ErrorContext(r.Context(), "save preset", slog.Any("error", err))
		f.Failure, f.Broken = "The preset could not be saved. Try again later.", true
	}
}

// presetForm is the state of the preset form.
type presetForm struct {
	ID      string // empty: a new preset
	Name    string // current name of an edited preset
	Version int
	Values  map[string]string
	Errors  map[string]string
	Notice  string
	Failure string
	// Conflict and Broken select the status of a failed save.
	Conflict, Broken bool
}

func (f *presetForm) setError(field, msg string) {
	if f.Errors == nil {
		f.Errors = map[string]string{}
	}

	if strings.HasPrefix(field, "tags.") {
		field = "tags"
	}

	if _, ok := f.Errors[field]; !ok {
		f.Errors[field] = msg
	}
}

func (f *presetForm) status() int {
	switch {
	case f.Broken:
		return http.StatusInternalServerError
	case f.Conflict:
		return http.StatusConflict
	default:
		return http.StatusUnprocessableEntity
	}
}

// formField is one input of the preset form.
type formField struct {
	Name, Label, Help string
	Numeric, Required bool
}

var formFields = []formField{
	{Name: "name", Label: "Name", Required: true, Help: "Shown in the receiver's preset list (at most 128 characters)."},
	{Name: "center_freq", Label: "Centre frequency (Hz)", Numeric: true, Required: true, Help: "Centre of the capture band, in Hz (for example 145000000)."},
	{Name: "samp_rate", Label: "Sample rate (S/s)", Numeric: true, Required: true,
		Help: "Width of the capture band. The preset fits only the devices that support this rate and whose range holds the band."},
	{Name: "start_freq", Label: "Start frequency (Hz)", Numeric: true, Help: "Frequency a listener starts on, within half the sample rate of the centre. Empty: the centre frequency."},
	{Name: "start_mod", Label: "Start mode", Help: "Demodulator a listener starts with, for example nfm, am, usb, lsb, cw or wfm."},
	{Name: "tuning_step", Label: "Tuning step (Hz)", Numeric: true, Help: "Empty: 1000 Hz."},
	{Name: "initial_squelch_level", Label: "Initial squelch (dBFS)", Numeric: true, Help: "Optional: -150 to 0."},
	{Name: "initial_nr_level", Label: "Initial noise reduction (dB)", Numeric: true, Help: "Optional: -20 to 20."},
	{Name: "description", Label: "Description", Help: "Optional (at most 1024 characters)."},
	{Name: "tags", Label: "Tags", Help: "Optional, separated by commas."},
	{Name: "slug", Label: "Slug", Help: "Optional: lower-case letters, digits and hyphens. Empty: derived from the name."},
}

func formValues(r *http.Request) map[string]string {
	out := make(map[string]string, len(formFields))
	for _, f := range formFields {
		out[f.Name] = strings.TrimSpace(r.PostForm.Get(f.Name))
	}

	return out
}

// draftOf reads the form into a draft; ok is false when a number does not
// parse (the errors are on the form).
func draftOf(f *presetForm) (Draft, bool) {
	v := f.Values
	d := Draft{Slug: v["slug"], Name: v["name"], Description: v["description"], StartMod: v["start_mod"]}

	for t := range strings.SplitSeq(v["tags"], ",") {
		if t = strings.TrimSpace(t); t != "" {
			d.Tags = append(d.Tags, t)
		}
	}

	num := func(name string) *int64 {
		s := v[name]
		if s == "" {
			return nil
		}

		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			f.setError(name, "Enter a whole number.")

			return nil
		}

		return &n
	}

	small := func(name string) *int {
		n := num(name)
		if n == nil {
			return nil
		}

		i := int(max(min(*n, 1<<20), -(1 << 20)))

		return &i
	}

	if c := num("center_freq"); c != nil {
		d.CenterFreq = *c
	}

	if c := num("samp_rate"); c != nil {
		d.SampRate = *c
	}

	d.StartFreq, d.TuningStep = num("start_freq"), num("tuning_step")
	d.InitialSquelchLevel, d.InitialNRLevel = small("initial_squelch_level"), small("initial_nr_level")

	return d, len(f.Errors) == 0
}

// formOf fills the form with a stored preset.
func formOf(p *Preset, notice string) presetForm {
	i64 := func(n int64) string { return strconv.FormatInt(n, 10) }
	v := map[string]string{
		"name": p.Name(), "slug": p.Slug(), "description": p.Description(), "tags": strings.Join(p.Tags(), ", "),
		"center_freq": i64(p.CenterFreq()), "samp_rate": i64(p.SampRate()), "start_freq": i64(p.StartFreq()),
		"start_mod": p.StartMod(), "tuning_step": i64(p.TuningStep()),
	}

	if q, ok := p.InitialSquelchLevel(); ok {
		v["initial_squelch_level"] = strconv.Itoa(q)
	}

	if n, ok := p.InitialNRLevel(); ok {
		v["initial_nr_level"] = strconv.Itoa(n)
	}

	return presetForm{ID: p.ID().String(), Name: p.Name(), Version: p.Version(), Values: v, Notice: notice}
}

// sentence capitalises a message and ends it with a full stop.
func sentence(s string) string {
	if s == "" {
		return s
	}

	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}

	return s
}

// formBodyLimit bounds the preset forms.
const formBodyLimit = 16 << 10

func parseForm(w http.ResponseWriter, r *http.Request, render Renderer) bool {
	r.Body = http.MaxBytesReader(w, r.Body, formBodyLimit)
	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest

		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}

		render.Error(w, r, status)

		return false
	}

	return true
}

// redirect answers a successful form: 303, or HX-Redirect for htmx.
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", path)
		w.WriteHeader(http.StatusNoContent)

		return
	}

	http.Redirect(w, r, path, http.StatusSeeOther)
}

// formatHz formats a frequency in Hz for people (kHz or MHz).
func formatHz(hz int64) string {
	switch {
	case hz >= 1_000_000:
		return strconv.FormatFloat(float64(hz)/1e6, 'f', -1, 64) + " MHz"
	case hz >= 1_000:
		return strconv.FormatFloat(float64(hz)/1e3, 'f', -1, 64) + " kHz"
	default:
		return strconv.FormatInt(hz, 10) + " Hz"
	}
}
