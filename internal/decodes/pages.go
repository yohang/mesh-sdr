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
)

//go:generate go tool templ generate

// Path is the Decodes page (FEATURE_SPEC §10.11).
const Path = "/decodes"

// PageSize is the number of messages of one page.
const PageSize = 50

// timeLayout is the form value of the time filters (datetime-local, UTC).
const timeLayout = "2006-01-02T15:04"

// Middlewares implements internal/http.Module.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module.
func (m *Module) Routes(r chi.Router) {
	r.Get(Path, m.listPage)
	r.Head(Path, m.listPage)
}

// filter is the filter form of the page; its fields are the query
// parameters.
type filter struct {
	Mode, Device, From, Until string
	Before                    int64
	// Errors are the invalid fields, by name.
	Errors map[string]string
}

// query returns the filter as a query string, before set to before.
func (f filter) query(before int64) string {
	q := url.Values{}

	for k, v := range map[string]string{"mode": f.Mode, "device": f.Device, "from": f.From, "until": f.Until} {
		if v != "" {
			q.Set(k, v)
		}
	}

	if before > 0 {
		q.Set("before", strconv.FormatInt(before, 10))
	}

	return q.Encode()
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
	// Next is the query of the next (older) page, "" on the last one.
	Next string
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
	f := filter{Mode: q.Get("mode"), Device: q.Get("device"), From: q.Get("from"), Until: q.Get("until"), Errors: map[string]string{}}

	var from, until time.Time

	if f.From != "" {
		t, err := time.ParseInLocation(timeLayout, f.From, time.UTC)
		if err != nil {
			f.Errors["from"] = "Enter a date and time."
		}

		from = t
	}

	if f.Until != "" {
		t, err := time.ParseInLocation(timeLayout, f.Until, time.UTC)
		if err != nil {
			f.Errors["until"] = "Enter a date and time."
		}

		until = t
	}

	if b := q.Get("before"); b != "" {
		f.Before, _ = strconv.ParseInt(b, 10, 64)
	}

	return f, from, until
}

// listPage serves the Decodes page: the messages of the devices the visitor
// may listen to, newest first, with mode, device and time filters.
func (m *Module) listPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, from, until := parseFilter(r.URL.Query())

	devices, err := m.d.Visible(ctx)
	if err != nil {
		m.d.Logger.ErrorContext(ctx, "decodes: visible devices", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	v := listView{Filter: f, Devices: devices, Modes: m.d.Modes, SignedIn: m.d.SignedIn(ctx), Rows: []Entry{}}

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

	if len(f.Errors) == 0 {
		rows, err := m.repo.List(ctx, Filter{
			Devices: ids, Mode: f.Mode, Device: f.Device, From: from, Until: until, Before: f.Before, Limit: PageSize + 1,
		})
		if err != nil {
			m.d.Logger.ErrorContext(ctx, "decodes: list", slog.Any("error", err))
			m.d.Render.Error(w, r, http.StatusInternalServerError)

			return
		}

		if len(rows) > PageSize {
			rows = rows[:PageSize]
			v.Next = f.query(rows[len(rows)-1].ID)
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
