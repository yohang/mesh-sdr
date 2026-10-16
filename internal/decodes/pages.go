package decodes

import (
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

//go:generate go tool templ generate

// Path is the Decodes page (FEATURE_SPEC §10.11).
const Path = "/decodes"

// PageSize is the number of messages of one page.
const PageSize = 50

// Middlewares implements internal/http.Module.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module.
func (m *Module) Routes(r chi.Router) {
	r.Get(Path, m.listPage)
}

// filter is the filter form of the page; its fields are the query
// parameters.
type filter struct {
	Mode, Device, From, To string
	// Errors are the invalid fields, by name.
	Errors map[string]string
}

// query returns the filter as a URL query.
func (f filter) query() url.Values {
	q := url.Values{}

	for k, v := range map[string]string{"mode": f.Mode, "device": f.Device, "from": f.From, "to": f.To} {
		if v != "" {
			q.Set(k, v)
		}
	}

	return q
}

// listView is the Decodes page.
type listView struct {
	Filter   filter
	Devices  []Device
	Modes    []Mode
	Rows     []Entry
	SignedIn bool
	// Retention is the retention notice ("Decodes are kept for N days").
	Retention string
	Paging    layout.Paging
	// MapLink returns the Map link of a message ("" for none).
	MapLink func(m Message) string
}

// mapLink returns the Map link of a message, "" for none.
func (v listView) mapLink(m Message) string {
	if v.MapLink == nil {
		return ""
	}

	return v.MapLink(m)
}

// deviceName names a device of the view.
func (v listView) deviceName(id string) string {
	for _, d := range v.Devices {
		if d.ID == id {
			return d.Name
		}
	}

	return id
}

func parseFilter(q url.Values) (filter, time.Time, time.Time) {
	f := filter{Mode: q.Get("mode"), Device: q.Get("device"), From: q.Get("from"), To: q.Get("to"), Errors: map[string]string{}}

	at := func(name, value string) time.Time {
		if value == "" {
			return time.Time{}
		}

		t, err := time.ParseInLocation(layout.DateTimeLocal, value, time.UTC)
		if err != nil {
			f.Errors[name] = "Enter a date and time."
		}

		return t
	}

	return f, at("from", f.From), at("to", f.To)
}

// listPage serves the Decodes page: the messages of the devices the visitor
// may listen to, newest first, with mode, device and time filters.
func (m *Module) listPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	f, from, to := parseFilter(q)

	devices, err := m.d.Visible(ctx)
	if err != nil {
		m.d.Logger.ErrorContext(ctx, "list visible devices", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	v := listView{Filter: f, Devices: devices, Modes: m.d.Modes, SignedIn: m.d.SignedIn(ctx), Rows: []Entry{}, MapLink: m.d.MapLink}

	if d := m.d.Retention(); d > 0 {
		v.Retention = "Decoded messages are kept for " + strconv.Itoa(int(d.Hours()/24)) + " days."
	}

	ids := make([]string, 0, len(devices))
	for _, d := range devices {
		ids = append(ids, d.ID)
	}

	status := http.StatusOK

	switch {
	case len(f.Errors) > 0:
		status = http.StatusBadRequest
	case f.Device != "" && !slices.Contains(ids, f.Device):
		f.Device = ""
		v.Filter = f
	}

	v.Paging = layout.Paging{
		Label: "Pages of decoded messages", Prev: "Newer messages", Next: "Older messages", Path: Path,
		Query: f.query(), Page: render.PageOf(q),
	}

	if len(f.Errors) == 0 {
		rows, err := m.repo.List(ctx, Filter{
			Devices: ids, Mode: f.Mode, Device: f.Device, From: from, To: to,
			Offset: (v.Paging.Page - 1) * PageSize, Limit: PageSize + 1,
		})
		if err != nil {
			m.d.Logger.ErrorContext(ctx, "list decoded messages", slog.Any("error", err))
			m.d.Render.Error(w, r, http.StatusInternalServerError)

			return
		}

		if len(rows) > PageSize {
			rows, v.Paging.More = rows[:PageSize], true
		}

		// JS8 frames are grouped into threads (DEC-030).
		v.Rows = Threads(rows)
	}

	m.d.Render.Page(w, r, status, layout.Page{Title: "Decodes", Section: layout.SectionDecodes}, listPage(v), nil)
}

// freqText shows a frequency in MHz ("—" when unknown).
func freqText(hz int64) string {
	if hz <= 0 {
		return "—"
	}

	return strconv.FormatFloat(float64(hz)/1e6, 'f', 6, 64)
}
