package bookmarks

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

//go:generate go tool templ generate

// ManagePath is the Bookmarks › Manage page (BMK-005).
const ManagePath = "/bookmarks/manage"

// Middlewares implements internal/http.Module.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module: Bookmarks › Manage, for
// operators and admins. Every change is an HTML form (ADR 0023); rows are
// edited inline with htmx and the pages work without it.
func (m *Module) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(m.d.Guard, noIndex)

		for path, h := range map[string]http.HandlerFunc{
			ManagePath: m.listPage, ManagePath + "/{id}": m.editPage, ManagePath + "/{id}/row": m.rowPage,
			ManagePath + "/{id}/delete": m.deletePage,
		} {
			r.Get(path, h)
			r.Head(path, h)
		}

		r.Post(ManagePath, m.create)
		r.Post(ManagePath+"/{id}", m.update)
		r.Post(ManagePath+"/{id}/delete", m.delete)
	})
}

func noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex")
		next.ServeHTTP(w, r)
	})
}

func (m *Module) page(w http.ResponseWriter, r *http.Request, status int, title string, content, fragment templ.Component) {
	sections := layout.OperatorAdminSections
	if m.d.AdminSections != nil {
		sections = m.d.AdminSections(r)
	}

	m.d.Render.Page(w, r, status, layout.Page{Title: title, Section: layout.SectionAdmin},
		layout.AdminPageWith("bookmarks", sections, content), fragment)
}

// Notices shown after a redirect (?done=…).
var notices = map[string]string{
	"created": "Bookmark added.",
	"saved":   "Bookmark saved.",
	"deleted": "Bookmark deleted.",
}

// names are the display names of the devices and presets.
type names struct {
	Devices []Device
	Presets []Preset
}

func (n names) device(id shared.DeviceID) string {
	for _, d := range n.Devices {
		if d.ID == id {
			return d.Name
		}
	}

	return id.String() + " (unknown device)"
}

func (n names) preset(id shared.UUID) string {
	for _, p := range n.Presets {
		if p.ID == id {
			return p.Name
		}
	}

	return "Unknown preset"
}

func (m *Module) names(ctx context.Context) (names, error) {
	var (
		n   names
		err error
	)

	if m.d.Devices != nil {
		if n.Devices, err = m.d.Devices.Devices(ctx); err != nil {
			return names{}, err
		}
	}

	if m.d.Presets != nil {
		if n.Presets, err = m.d.Presets.Presets(ctx); err != nil {
			return names{}, err
		}
	}

	return n, nil
}

// listView is the Bookmarks › Manage page.
type listView struct {
	Names     names
	Filter    filterForm
	Bands     []Band
	Region    string
	Rows      []*Bookmark
	Add       bookmarkForm
	Notice    string
	Truncated bool
}

// filterForm holds the filters of the table, as typed.
type filterForm struct {
	Origin, Device, Scope, Band, From, To string
	Errors                                map[string]string
}

// active reports whether a filter is set.
func (f filterForm) active() bool {
	return f.Origin != "" || f.Device != "" || f.Scope != "" || f.Band != "" || f.From != "" || f.To != ""
}

// filterOf reads the filters of the query.
func (m *Module) filterOf(q url.Values) (filterForm, Filter) {
	ff := filterForm{
		Origin: q.Get("origin"), Device: q.Get("device"), Scope: q.Get("scope"), Band: q.Get("band"),
		From: strings.TrimSpace(q.Get("from")), To: strings.TrimSpace(q.Get("to")),
	}

	var f Filter

	switch Origin(ff.Origin) {
	case OriginDB, OriginBuiltin:
		f.Origin = Origin(ff.Origin)
	default:
		ff.Origin = ""
	}

	switch ScopeKind(ff.Scope) {
	case ScopeAll, ScopeDevice, ScopePreset:
		f.Scope = ScopeKind(ff.Scope)
	default:
		ff.Scope = ""
	}

	f.Device = ff.Device

	if ff.Band != "" {
		for _, b := range m.Bandplan().Bands(0, 1<<62) {
			if b.Name == ff.Band {
				f.Range, _ = NewRange(&b.Low, &b.High)

				return ff, f
			}
		}

		ff.Band = ""
	}

	num := func(name, s string) *int64 {
		if s == "" {
			return nil
		}

		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			if ff.Errors == nil {
				ff.Errors = map[string]string{}
			}

			ff.Errors[name] = "Enter a frequency in Hz."

			return nil
		}

		return &n
	}

	from, to := num("from", ff.From), num("to", ff.To)
	if from != nil || to != nil {
		rg, err := NewRange(from, to)
		if err != nil {
			if ff.Errors == nil {
				ff.Errors = map[string]string{}
			}

			ff.Errors["to"] = "Enter a highest frequency not below the lowest one."
		} else {
			f.Range = rg
		}
	}

	return ff, f
}

// maxRows bounds the rows of the table: a filter narrows the rest.
const maxRows = 1000

func (m *Module) listView(r *http.Request, add bookmarkForm) (listView, error) {
	ctx := r.Context()
	ff, f := m.filterOf(r.URL.Query())

	n, err := m.names(ctx)
	if err != nil {
		return listView{}, err
	}

	rows, err := m.List(ctx, f)
	if err != nil {
		return listView{}, err
	}

	v := listView{
		Names: n, Filter: ff, Bands: m.Bandplan().Bands(0, 1<<62), Region: m.Region(), Add: add,
		Notice: notices[r.URL.Query().Get("done")],
	}

	if len(rows) > maxRows {
		rows, v.Truncated = rows[:maxRows], true
	}

	v.Rows = rows

	return v, nil
}

func (m *Module) listPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	add := bookmarkForm{Values: map[string]string{
		"name": q.Get("name"), "frequency": q.Get("f"), "modulation": q.Get("m"), "underlying": q.Get("m2"),
		"scope": string(ScopeAll), "scannable": "on",
	}}

	m.renderList(w, r, http.StatusOK, add)
}

func (m *Module) renderList(w http.ResponseWriter, r *http.Request, status int, add bookmarkForm) {
	v, err := m.listView(r, add)
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "list bookmarks", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	m.page(w, r, status, "Bookmarks", listPage(v), nil)
}

// bookmark returns the bookmark of the path, or answers 404 (or 500).
func (m *Module) bookmark(w http.ResponseWriter, r *http.Request) (*Bookmark, bool) {
	b, err := m.Get(r.Context(), chi.URLParam(r, "id"))

	switch {
	case errors.Is(err, ErrBookmarkNotFound):
		m.d.Render.Error(w, r, http.StatusNotFound)

		return nil, false
	case err != nil:
		m.d.Logger.ErrorContext(r.Context(), "read bookmark", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return nil, false
	}

	return b, true
}

func (m *Module) editPage(w http.ResponseWriter, r *http.Request) {
	b, ok := m.bookmark(w, r)
	if !ok {
		return
	}

	m.renderEdit(w, r, http.StatusOK, b, formOf(b))
}

// renderEdit answers the edit form of a bookmark: the inline row for htmx,
// the edit page otherwise. A pack bookmark shows read-only.
func (m *Module) renderEdit(w http.ResponseWriter, r *http.Request, status int, b *Bookmark, f bookmarkForm) {
	n, err := m.names(r.Context())
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "bookmark names", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	if b.ReadOnly() {
		m.page(w, r, status, b.Name(), packPage(b), viewRow(b, n))

		return
	}

	m.page(w, r, status, "Edit "+b.Name(), editPage(f, n), editRow(f, n))
}

// rowPage answers the table row of a bookmark (the cancel of an inline
// edit); a full page request goes back to the table.
func (m *Module) rowPage(w http.ResponseWriter, r *http.Request) {
	b, ok := m.bookmark(w, r)
	if !ok {
		return
	}

	if !render.WantsFragment(r) {
		http.Redirect(w, r, ManagePath, http.StatusSeeOther)

		return
	}

	n, err := m.names(r.Context())
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "bookmark names", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	m.page(w, r, http.StatusOK, b.Name(), nil, viewRow(b, n))
}

func (m *Module) deletePage(w http.ResponseWriter, r *http.Request) {
	b, ok := m.bookmark(w, r)
	if !ok {
		return
	}

	if b.ReadOnly() {
		m.d.Render.Error(w, r, http.StatusConflict)

		return
	}

	m.page(w, r, http.StatusOK, "Delete "+b.Name(), deletePage(b, ""), nil)
}

func (m *Module) create(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r, m.d.Render) {
		return
	}

	f := bookmarkForm{Values: formValues(r)}

	if d, ok := f.draft(); ok {
		b, err := m.Create(r.Context(), d)
		if err == nil {
			redirect(w, r, ManagePath+"?done=created#bookmark-"+b.ID().String())

			return
		}

		m.formError(r, &f, err)
	}

	m.renderList(w, r, f.status(), f)
}

func (m *Module) update(w http.ResponseWriter, r *http.Request) {
	cur, ok := m.bookmark(w, r)
	if !ok || !parseForm(w, r, m.d.Render) {
		return
	}

	if cur.ReadOnly() {
		m.renderEdit(w, r, http.StatusConflict, cur, formOf(cur))

		return
	}

	version, err := strconv.Atoi(r.PostForm.Get("version"))
	if err != nil {
		m.d.Render.Error(w, r, http.StatusBadRequest)

		return
	}

	f := bookmarkForm{ID: cur.ID().String(), Name: cur.Name(), Version: version, Values: formValues(r)}

	if d, ok := f.draft(); ok {
		b, err := m.Update(r.Context(), f.ID, version, d)
		if err == nil {
			if render.WantsFragment(r) {
				n, nerr := m.names(r.Context())
				if nerr != nil {
					m.d.Logger.ErrorContext(r.Context(), "bookmark names", slog.Any("error", nerr))
				}

				m.page(w, r, http.StatusOK, b.Name(), nil, viewRow(b, n))

				return
			}

			redirect(w, r, ManagePath+"?done=saved#bookmark-"+b.ID().String())

			return
		}

		m.formError(r, &f, err)
	}

	m.renderEdit(w, r, f.status(), cur, f)
}

func (m *Module) delete(w http.ResponseWriter, r *http.Request) {
	b, ok := m.bookmark(w, r)
	if !ok || !parseForm(w, r, m.d.Render) {
		return
	}

	version, verr := strconv.Atoi(r.PostForm.Get("version"))
	if verr != nil {
		m.d.Render.Error(w, r, http.StatusBadRequest)

		return
	}

	err := m.Delete(r.Context(), b.ID().String(), version)
	if err == nil {
		redirect(w, r, ManagePath+"?done=deleted")

		return
	}

	status, msg := http.StatusConflict, ""

	switch {
	case errors.Is(err, ErrReadOnly):
		msg = "Pack bookmarks are read-only."
	case errors.Is(err, ErrVersionConflict):
		msg = "This bookmark was changed meanwhile: check it before deleting it."
	case errors.Is(err, ErrBookmarkNotFound):
		m.d.Render.Error(w, r, http.StatusNotFound)

		return
	default:
		m.d.Logger.ErrorContext(r.Context(), "delete bookmark", slog.String("bookmark_id", b.ID().String()), slog.Any("error", err))
		status, msg = http.StatusInternalServerError, "The bookmark could not be deleted. Try again later."
	}

	m.page(w, r, status, "Delete "+b.Name(), deletePage(b, msg), nil)
}

// formError records a refused save on the form.
func (m *Module) formError(r *http.Request, f *bookmarkForm, err error) {
	var de *shared.Error

	switch {
	case errors.Is(err, ErrInvalidBookmark) && errors.As(err, &de):
		for _, v := range de.Violations() {
			f.setError(v.Path(), sentence(v.Message()))
		}

		if len(f.Errors) == 0 {
			f.Failure = sentence(de.Message())
		}
	case errors.Is(err, ErrDuplicate):
		f.setError("name", "A bookmark with this name, frequency and mode exists.")
	case errors.Is(err, ErrVersionConflict):
		f.Failure, f.Conflict = "This bookmark was changed meanwhile: reload the page to edit the current version.", true
	case errors.Is(err, ErrBookmarkNotFound):
		f.Failure, f.Conflict = "This bookmark no longer exists.", true
	case errors.Is(err, ErrReadOnly):
		f.Failure, f.Conflict = "Pack bookmarks are read-only.", true
	default:
		m.d.Logger.ErrorContext(r.Context(), "save bookmark", slog.Any("error", err))
		f.Failure, f.Broken = "The bookmark could not be saved. Try again later.", true
	}
}

// bookmarkForm is the state of a bookmark form (add, or edit of one).
type bookmarkForm struct {
	ID      string // empty: a new bookmark
	Name    string // current name of an edited bookmark
	Version int
	Values  map[string]string
	Errors  map[string]string
	Failure string
	// Conflict and Broken select the status of a failed save.
	Conflict, Broken bool
}

func (f *bookmarkForm) setError(field, msg string) {
	if f.Errors == nil {
		f.Errors = map[string]string{}
	}

	if _, ok := f.Errors[field]; !ok {
		f.Errors[field] = msg
	}
}

func (f *bookmarkForm) status() int {
	switch {
	case f.Broken:
		return http.StatusInternalServerError
	case f.Conflict:
		return http.StatusConflict
	default:
		return http.StatusUnprocessableEntity
	}
}

// formKeys are the posted fields of the bookmark form.
var formKeys = []string{"name", "frequency", "modulation", "underlying", "description", "scannable", "scope", "device", "preset"}

func formValues(r *http.Request) map[string]string {
	out := make(map[string]string, len(formKeys))
	for _, k := range formKeys {
		out[k] = strings.TrimSpace(r.PostForm.Get(k))
	}

	return out
}

// draft reads the form; ok is false when a field does not parse (the
// errors are on the form).
func (f *bookmarkForm) draft() (Draft, bool) {
	v := f.Values
	d := Draft{
		Name: v["name"], Modulation: v["modulation"], Underlying: v["underlying"], Description: v["description"],
		Scannable: v["scannable"] != "",
	}

	if s := v["frequency"]; s == "" {
		f.setError("frequency", "Required.")
	} else {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			f.setError("frequency", "Enter a whole number of Hz.")
		}

		d.Frequency = n
	}

	var err error

	switch ScopeKind(v["scope"]) {
	case ScopeDevice:
		dev, derr := shared.NewDeviceID(v["device"])
		if derr != nil {
			f.setError("device", "Choose a device.")
		} else if d.Scope, err = OnDevice(dev); err != nil {
			f.setError("device", "Choose a device.")
		}
	case ScopePreset:
		p, perr := shared.ParseUUID(v["preset"])
		if perr != nil {
			f.setError("preset", "Choose a preset.")
		} else if d.Scope, err = OnPreset(p); err != nil {
			f.setError("preset", "Choose a preset.")
		}
	case ScopeAll:
		d.Scope = AllDevices()
	default:
		f.setError("scope", "Choose where the bookmark shows.")
	}

	return d, len(f.Errors) == 0
}

// formOf fills the form with a stored bookmark.
func formOf(b *Bookmark) bookmarkForm {
	v := map[string]string{
		"name": b.Name(), "frequency": strconv.FormatInt(b.Frequency(), 10), "modulation": b.Modulation(),
		"underlying": b.Underlying(), "description": b.Description(), "scope": string(b.Scope().Kind()),
		"device": b.Scope().Device().String(),
	}

	if b.Scannable() {
		v["scannable"] = "on"
	}

	if p := b.Scope().Preset(); !p.IsZero() {
		v["preset"] = p.String()
	}

	return bookmarkForm{ID: b.ID().String(), Name: b.Name(), Version: b.Version(), Values: v}
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

// formBodyLimit bounds the bookmark forms.
const formBodyLimit = 16 << 10

func parseForm(w http.ResponseWriter, r *http.Request, rd Renderer) bool {
	r.Body = http.MaxBytesReader(w, r.Body, formBodyLimit)
	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest

		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}

		rd.Error(w, r, status)

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

// FormatHz formats a frequency in Hz for people (kHz or MHz).
func FormatHz(hz int64) string {
	switch {
	case hz >= 1_000_000:
		return strconv.FormatFloat(float64(hz)/1e6, 'f', -1, 64) + " MHz"
	case hz >= 1_000:
		return strconv.FormatFloat(float64(hz)/1e3, 'f', -1, 64) + " kHz"
	default:
		return strconv.FormatInt(hz, 10) + " Hz"
	}
}
