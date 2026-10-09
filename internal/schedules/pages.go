package schedules

import (
	"context"
	"log/slog"
	"net/http"

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

// PagesDeps are the dependencies of the admin page.
type PagesDeps struct {
	Render  Renderer
	Guard   func(http.Handler) http.Handler // admin role and network (identity)
	Service *Service
	// PresetName names a preset ("" when unknown).
	PresetName func(ctx context.Context, id shared.UUID) string
	Logger     *slog.Logger
}

// Pages serves Admin › Schedules: a read-only list of the schedules (ADR
// 0026). It is an internal/http.Module.
type Pages struct{ d PagesDeps }

// NewPages returns the module.
func NewPages(d PagesDeps) *Pages { return &Pages{d: d} }

// Middlewares implements internal/http.Module.
func (m *Pages) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module.
func (m *Pages) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(m.d.Guard, noIndex)
		r.Get("/admin/schedules", m.list)
	})
}

func noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex")
		next.ServeHTTP(w, r)
	})
}

// scheduleRow is one schedule as the list shows it.
type scheduleRow struct {
	Device, Preset, Window, Days string
	Priority                     int
	Enabled                      bool
	// Disabled explains why the hub disabled the schedule (empty: it did
	// not).
	Disabled string
}

func (m *Pages) list(w http.ResponseWriter, r *http.Request) {
	list, err := m.d.Service.List(r.Context())
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "list schedules", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	rows := make([]scheduleRow, 0, len(list))

	for _, s := range list {
		row := scheduleRow{
			Device: s.Device().String(), Preset: m.d.PresetName(r.Context(), s.Preset()), Window: s.Window().String(),
			Days: s.Days().String(), Priority: s.Priority().Int(), Enabled: s.Enabled(),
		}

		if row.Preset == "" {
			row.Preset = s.Preset().String()
		}

		if reason, _ := s.DisabledReason(); reason != ReasonNone {
			row.Disabled = explain(reason)
		}

		rows = append(rows, row)
	}

	m.d.Render.Page(w, r, http.StatusOK, layout.Page{Title: "Schedules", Section: layout.SectionAdmin},
		layout.AdminPage("schedules", listPage(rows)), nil)
}

// explain says why the hub disabled a schedule.
func explain(reason DisabledReason) string {
	switch reason {
	case ReasonDeviceStale:
		return "disabled: the node no longer reports the device"
	case ReasonDeviceRemoved:
		return "disabled: the device was removed from the registry"
	case ReasonPresetIncompatible:
		return "disabled: the preset no longer fits the device"
	default:
		return "disabled: " + string(reason)
	}
}
