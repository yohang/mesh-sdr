package files

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// GalleryPath is the Files section (FIL-001).
const GalleryPath = "/files"

// PageSize is the number of files of a gallery page.
const PageSize = 48

// Audit actions of the deletions (FIL-003).
const (
	ActionDelete     = "file.delete"
	ActionBulkDelete = "file.delete_bulk"
)

// Renderer renders pages in the app shell (internal/web/render).
type Renderer interface {
	Page(w http.ResponseWriter, r *http.Request, status int, page layout.Page, content, fragment templ.Component)
	Error(w http.ResponseWriter, r *http.Request, status int)
}

// GalleryDeps are the dependencies of the Files section.
type GalleryDeps struct {
	Repo   *Files
	Tx     *db.DB
	Audit  audit.Appender
	Render Renderer
	// Visibility returns the files the caller of ctx may see (listen
	// policy, ADR 0026).
	Visibility func(ctx context.Context) Access
	// Listener admits signed-in users and sends anonymous visitors to the
	// sign-in page: it guards the section when the visitor may see no file.
	Listener func(http.Handler) http.Handler
	// Operator guards the deletion of a file (operators and admins);
	// Admin the deletion by filter (admins, admin.allowed_networks).
	Operator, Admin func(http.Handler) http.Handler
	// CanDelete and CanBulkDelete tell the pages which delete forms to
	// show; the guards decide.
	CanDelete, CanBulkDelete func(ctx context.Context) bool
	// Policy returns the current retention policy (notice of the gallery).
	Policy func() RetentionPolicy
	Logger *slog.Logger
}

// Gallery is the Files section (FIL-001, FIL-003, FIL-007): the gallery of
// the files the nodes sent, their detail view, their deletion, and their
// content and thumbnail on the JSON API (FIL-002). It is an
// internal/http.Module.
type Gallery struct{ d GalleryDeps }

// NewGallery returns the section.
func NewGallery(d GalleryDeps) *Gallery { return &Gallery{d: d} }

// Middlewares implements internal/http.Module.
func (g *Gallery) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module.
func (g *Gallery) Routes(r chi.Router) {
	for path, h := range map[string]http.HandlerFunc{GalleryPath: g.list, GalleryPath + "/{id}": g.detail} {
		h := g.gate(h)
		r.Get(path, h)
		r.Head(path, h)
	}

	r.With(g.d.Operator).Post(GalleryPath+"/{id}/delete", g.delete)
	r.With(g.d.Admin).Post(GalleryPath+"/delete", g.deleteMatching)
}

// gate sends a visitor who may see no file to the sign-in page.
func (g *Gallery) gate(next http.HandlerFunc) http.HandlerFunc {
	guarded := g.d.Listener(next)

	return func(w http.ResponseWriter, r *http.Request) {
		if g.d.Visibility(r.Context()).Denied {
			guarded.ServeHTTP(w, r)

			return
		}

		next(w, r)
	}
}

// Visible returns a complete file the caller of ctx may see, or
// ErrFileNotFound.
func (g *Gallery) Visible(ctx context.Context, id string) (Entry, error) {
	uid, err := shared.ParseUUID(id)
	if err != nil {
		return Entry{}, ErrFileNotFound
	}

	e, err := g.d.Repo.Entry(ctx, uid)
	if err != nil {
		return Entry{}, err
	}

	if !g.d.Visibility(ctx).Allows(e.DeviceID) {
		return Entry{}, ErrFileNotFound
	}

	return e, nil
}

// Delete deletes one file (FIL-003), audited.
func (g *Gallery) Delete(ctx context.Context, id shared.UUID) error {
	return g.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		e, err := g.d.Repo.Entry(ctx, id)
		if err != nil {
			return err
		}

		if _, err := g.d.Repo.remove(ctx, id); err != nil {
			return err
		}

		return g.d.Audit.Append(ctx, audit.Record{
			Action: ActionDelete, TargetType: "file", TargetID: id.String(),
			Before: map[string]string{"name": e.Name, "kind": string(e.Kind), "device_id": e.DeviceID},
		})
	})
}

// DeleteMatching deletes every file matching f (FIL-003, admins), audited;
// it returns how many.
func (g *Gallery) DeleteMatching(ctx context.Context, f Filter) (int64, error) {
	var n int64

	err := g.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		var err error

		if n, err = g.d.Repo.deleteMatching(ctx, f); err != nil {
			return err
		}

		after := filterValues(f)
		after["deleted"] = strconv.FormatInt(n, 10)

		return g.d.Audit.Append(ctx, audit.Record{Action: ActionBulkDelete, TargetType: "files", After: after})
	})

	return n, err
}

// filterForm holds the gallery filters as typed, with their errors.
type filterForm struct {
	Media, Device, Mode, From, To, FreqMin, FreqMax string
	Errors                                          map[string]string
}

// active reports whether a filter is set.
func (f filterForm) active() bool {
	return f.Media != "" || f.Device != "" || f.Mode != "" || f.From != "" || f.To != "" || f.FreqMin != "" || f.FreqMax != ""
}

// query returns the filters as a URL query.
func (f filterForm) query() url.Values {
	q := url.Values{}

	for k, v := range map[string]string{
		"media": f.Media, "device": f.Device, "mode": f.Mode, "from": f.From, "to": f.To, "freq_min": f.FreqMin, "freq_max": f.FreqMax,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}

	return q
}

// dateTimeLocal is the value format of a datetime-local input.
const dateTimeLocal = "2006-01-02T15:04"

// parseFilter reads the filters of a query or form. Invalid values are
// dropped from the filter and reported.
func parseFilter(v url.Values) (filterForm, Filter) {
	ff := filterForm{
		Media: v.Get("media"), Device: strings.TrimSpace(v.Get("device")), Mode: strings.TrimSpace(v.Get("mode")),
		From: strings.TrimSpace(v.Get("from")), To: strings.TrimSpace(v.Get("to")),
		FreqMin: strings.TrimSpace(v.Get("freq_min")), FreqMax: strings.TrimSpace(v.Get("freq_max")),
	}

	var f Filter

	fail := func(name, msg string) {
		if ff.Errors == nil {
			ff.Errors = map[string]string{}
		}

		ff.Errors[name] = msg
	}

	// An invalid choice is dropped and reported: the gallery shows the
	// rest, a deletion refuses it.
	switch Media(ff.Media) {
	case "":
	case MediaImage, MediaAudio, MediaText:
		f.Media = Media(ff.Media)
	default:
		fail("media", "Choose a type.")

		ff.Media = ""
	}

	if _, err := shared.NewDeviceID(ff.Device); err == nil {
		f.DeviceID = ff.Device
	} else if ff.Device != "" {
		fail("device", "Choose a device.")

		ff.Device = ""
	}

	if modePattern.MatchString(ff.Mode) {
		f.Mode = ff.Mode
	} else if ff.Mode != "" {
		fail("mode", "Choose a decoder.")

		ff.Mode = ""
	}

	for _, t := range []struct {
		name, value string
		to          *time.Time
	}{{"from", ff.From, &f.From}, {"to", ff.To, &f.To}} {
		if t.value == "" {
			continue
		}

		at, err := time.ParseInLocation(dateTimeLocal, t.value, time.UTC)
		if err != nil {
			fail(t.name, "Enter a date and time (UTC).")

			continue
		}

		*t.to = at
	}

	if !f.From.IsZero() && !f.To.IsZero() && !f.To.After(f.From) {
		fail("to", "Enter an end after the start.")

		f.To = time.Time{}
	}

	for _, fr := range []struct {
		name, value string
		to          *int64
	}{{"freq_min", ff.FreqMin, &f.FreqMin}, {"freq_max", ff.FreqMax, &f.FreqMax}} {
		if fr.value == "" {
			continue
		}

		khz, err := strconv.ParseFloat(fr.value, 64)
		if err != nil || khz <= 0 || khz > 1e9 || math.IsNaN(khz) {
			fail(fr.name, "Enter a frequency in kHz.")

			continue
		}

		*fr.to = int64(math.Round(khz * 1000))
	}

	if f.FreqMin > 0 && f.FreqMax > 0 && f.FreqMax < f.FreqMin {
		fail("freq_max", "Enter a highest frequency not below the lowest one.")

		f.FreqMax = 0
	}

	return ff, f
}

// filterValues describes a filter for the audit log.
func filterValues(f Filter) map[string]string {
	out := map[string]string{}

	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}

	set("media", string(f.Media))
	set("device_id", f.DeviceID)
	set("mode", f.Mode)

	if !f.From.IsZero() {
		out["from"] = f.From.Format(time.RFC3339)
	}

	if !f.To.IsZero() {
		out["to"] = f.To.Format(time.RFC3339)
	}

	if f.FreqMin > 0 {
		out["freq_min_hz"] = strconv.FormatInt(f.FreqMin, 10)
	}

	if f.FreqMax > 0 {
		out["freq_max_hz"] = strconv.FormatInt(f.FreqMax, 10)
	}

	return out
}

// galleryView is the Files page.
type galleryView struct {
	Filter        filterForm
	Devices       []string
	Modes         []string
	Files         []Entry
	Page          int
	More          bool
	Notice        string
	Policy        string
	CanBulkDelete bool
}

// pageURL returns the gallery URL of page n with the current filters.
func (v galleryView) pageURL(n int) string {
	q := v.Filter.query()
	if n > 1 {
		q.Set("page", strconv.Itoa(n))
	}

	if len(q) == 0 {
		return GalleryPath
	}

	return GalleryPath + "?" + q.Encode()
}

// Notices shown after a redirect (?done=…).
var notices = map[string]string{"deleted": "The file was deleted."}

func (g *Gallery) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	ff, f := parseFilter(q)
	vis := g.d.Visibility(ctx)

	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 || page > 10_000 {
		page = 1
	}

	list, err := g.d.Repo.List(ctx, f, vis, (page-1)*PageSize, PageSize+1)
	if err != nil {
		g.d.Logger.ErrorContext(ctx, "list files", slog.Any("error", err))
		g.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	devices, modes, err := g.d.Repo.FilterValues(ctx)
	if err != nil {
		g.d.Logger.ErrorContext(ctx, "file filter values", slog.Any("error", err))
		g.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	visible := devices[:0]
	for _, d := range devices {
		if vis.Allows(d) {
			visible = append(visible, d)
		}
	}

	v := galleryView{
		Filter: ff, Devices: visible, Modes: modes, Files: list, Page: page, Policy: g.d.Policy().String(),
		Notice: notices[q.Get("done")], CanBulkDelete: g.d.CanBulkDelete(ctx),
	}

	if n := q.Get("deleted"); q.Get("done") == "bulk" && n != "" {
		if c, err := strconv.ParseInt(n, 10, 64); err == nil && c >= 0 {
			v.Notice = strconv.FormatInt(c, 10) + " files were deleted."
		}
	}

	if len(list) > PageSize {
		v.Files, v.More = list[:PageSize], true
	}

	g.d.Render.Page(w, r, http.StatusOK, layout.Page{Title: "Files", Section: layout.SectionFiles}, galleryPage(v), nil)
}

// detailView is the detail page of a file.
type detailView struct {
	File      Entry
	Text      string
	Truncated bool
	CanDelete bool
}

// MaxTextPreview bounds the text shown by the detail view.
const MaxTextPreview = 4 << 10

func (g *Gallery) detail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	e, err := g.Visible(ctx, chi.URLParam(r, "id"))

	switch {
	case errors.Is(err, ErrFileNotFound):
		g.d.Render.Error(w, r, http.StatusNotFound)

		return
	case err != nil:
		g.d.Logger.ErrorContext(ctx, "read file", slog.Any("error", err))
		g.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	v := detailView{File: e, CanDelete: g.d.CanDelete(ctx)}

	if e.IsText() {
		data, err := g.d.Repo.FirstChunk(ctx, e)
		if err != nil {
			g.d.Logger.ErrorContext(ctx, "read file content", slog.String("file_id", e.ID.String()), slog.Any("error", err))
			g.d.Render.Error(w, r, http.StatusInternalServerError)

			return
		}

		v.Text, v.Truncated = previewText(data)
	}

	g.d.Render.Page(w, r, http.StatusOK, layout.Page{Title: e.Name, Section: layout.SectionFiles}, detailPage(v), nil)
}

// previewText returns the start of a text, cut on a rune boundary.
func previewText(data []byte) (string, bool) {
	if len(data) <= MaxTextPreview {
		return string(data), false
	}

	return strings.ToValidUTF8(string(data[:MaxTextPreview]), ""), true
}

func (g *Gallery) delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, err := shared.ParseUUID(chi.URLParam(r, "id"))
	if err != nil {
		g.d.Render.Error(w, r, http.StatusNotFound)

		return
	}

	err = g.Delete(ctx, id)

	switch {
	case errors.Is(err, ErrFileNotFound):
		g.d.Render.Error(w, r, http.StatusNotFound)
	case err != nil:
		g.d.Logger.ErrorContext(ctx, "delete a file", slog.String("file_id", id.String()), slog.Any("error", err))
		g.d.Render.Error(w, r, http.StatusInternalServerError)
	default:
		redirect(w, r, GalleryPath+"?done=deleted")
	}
}

// formBodyLimit bounds the body of the delete forms.
const formBodyLimit = 8 << 10

func (g *Gallery) deleteMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, formBodyLimit)
	if err := r.ParseForm(); err != nil {
		g.d.Render.Error(w, r, http.StatusBadRequest)

		return
	}

	// Every filter must be valid (an ignored one would widen the
	// deletion), and deleting every file must be asked for explicitly.
	ff, f := parseFilter(r.PostForm)
	if len(ff.Errors) > 0 || (!ff.active() && r.PostForm.Get("all") != "1") {
		g.d.Render.Error(w, r, http.StatusUnprocessableEntity)

		return
	}

	n, err := g.DeleteMatching(ctx, f)
	if err != nil {
		g.d.Logger.ErrorContext(ctx, "delete files by filter", slog.Any("error", err))
		g.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	q := ff.query()
	q.Set("done", "bulk")
	q.Set("deleted", strconv.FormatInt(n, 10))
	redirect(w, r, GalleryPath+"?"+q.Encode())
}

// redirect sends a full-page redirect, for htmx (HX-Redirect) and plain
// requests (303).
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", path)
		w.WriteHeader(http.StatusNoContent)

		return
	}

	http.Redirect(w, r, path, http.StatusSeeOther)
}

// WriteContent writes the content of a file Visible returned to w.
func (g *Gallery) WriteContent(ctx context.Context, e Entry, w io.Writer) error {
	return g.d.Repo.WriteContent(ctx, e, w)
}

// Thumbnail returns the thumbnail of a file Visible returned, or
// ErrFileNotFound when it has none.
func (g *Gallery) Thumbnail(ctx context.Context, e Entry) ([]byte, error) {
	if !e.HasThumbnail {
		return nil, ErrFileNotFound
	}

	return g.d.Repo.Thumbnail(ctx, e.ID)
}

// decodesPath is the Decodes page (FEATURE_SPEC §10.11).
const decodesPath = "/decodes"

// decodesTimeLayout is the time filter of the Decodes page (UTC, to the
// minute).
const decodesTimeLayout = "2006-01-02T15:04"

// decodesURL links the decoded messages related to a file (FIL-007): the
// messages of its device and mode during its reception, a minute around.
func decodesURL(e Entry) string {
	end := e.ReceivedEnd
	if end.IsZero() {
		end = e.ReceivedStart
	}

	q := url.Values{}
	q.Set("mode", e.Mode)
	q.Set("device", e.DeviceID)
	q.Set("from", e.ReceivedStart.UTC().Add(-time.Minute).Format(decodesTimeLayout))
	q.Set("until", end.UTC().Add(2*time.Minute).Format(decodesTimeLayout))

	return decodesPath + "?" + q.Encode()
}
