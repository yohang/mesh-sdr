package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

//go:generate go tool templ generate

// Devices is the device registry.
type Devices interface {
	List(ctx context.Context) ([]*domain.Device, error)
	ListByNode(ctx context.Context, id domain.NodeID) ([]*domain.Device, error)
	Get(ctx context.Context, id string) (*domain.Device, error)
	Forget(ctx context.Context, actor audit.Actor, id string) error
}

// Renderer renders pages in the app shell (internal/web/render).
type Renderer interface {
	Page(w http.ResponseWriter, r *http.Request, status int, page layout.Page, content, fragment templ.Component)
	Error(w http.ResponseWriter, r *http.Request, status int)
}

// ScheduleRow is one schedule of a device, as the device page shows it.
type ScheduleRow struct {
	ID, Preset, Window, Days string
	Priority                 int
	Enabled                  bool
	// DisabledReason is set when the hub disabled the schedule (GRID-016).
	DisabledReason string
	DisabledAt     time.Time
}

// DeviceSchedules lists the schedules of a device (ADR 0020).
type DeviceSchedules interface {
	ForDevice(ctx context.Context, device string) ([]ScheduleRow, error)
}

// AdminDeps are the dependencies of the admin grid pages.
type AdminDeps struct {
	Render       Renderer
	Devices      Devices
	Nodes        NodeAdmin
	History      LoadHistory
	Capabilities CapabilityReports
	Connections  ConnectionRegistry
	Users        UserNames
	// Schedules lists the schedules of a device; nil shows none.
	Schedules DeviceSchedules
	// PresetName names a preset ("" when unknown); nil shows the id.
	PresetName func(ctx context.Context, id shared.UUID) string
	// PresetBand describes a preset (name, centre, sample rate); nil shows
	// no preset on Admin › Connections.
	PresetBand func(ctx context.Context, id shared.UUID) (PresetBand, bool)
	// MaskIPs reports privacy.mask_ips; nil masks.
	MaskIPs func(ctx context.Context) bool
	// Audit records the reveal of a masked address.
	Audit    audit.Appender
	Operator func(http.Handler) http.Handler // operator role (identity)
	Admin    func(http.Handler) http.Handler // admin role and network (identity)
	IsAdmin  func(r *http.Request) bool
	Now      func() time.Time
	Logger   *slog.Logger
}

// AdminModule serves the grid pages of the admin area: the read-only device
// pages (ADM-008) with "forget device" (ADM-009), Admin › Nodes (GRID-005,
// GRID-009) and Admin › Connections (GRID-017). Operators read devices and
// nodes; everything else is for admins.
type AdminModule struct{ d AdminDeps }

// NewAdminModule returns the router module (internal/http.Module).
func NewAdminModule(d AdminDeps) *AdminModule { return &AdminModule{d: d} }

// Middlewares implements internal/http.Module.
func (m *AdminModule) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module.
func (m *AdminModule) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(m.d.Operator, noIndex)
		r.Get("/admin/devices", m.list)
		r.Head("/admin/devices", m.list)
		r.Get("/admin/devices/{id}", m.detail)
		r.Head("/admin/devices/{id}", m.detail)
		r.Get("/admin/nodes", m.nodesPage)
		r.Head("/admin/nodes", m.nodesPage)
		r.Get("/admin/nodes/{id}", m.nodePage)
		r.Head("/admin/nodes/{id}", m.nodePage)
	})
	r.Group(func(r chi.Router) {
		r.Use(m.d.Admin, noIndex)
		r.Post("/admin/devices/{id}/forget", m.forget)
		r.Get("/admin/nodes/new", m.newNodePage)
		r.Post("/admin/nodes", m.addNode)
		r.Post("/admin/nodes/{id}", m.editNode)
		r.Post("/admin/nodes/{id}/disable", m.setDisabled(true))
		r.Post("/admin/nodes/{id}/enable", m.setDisabled(false))
		r.Post("/admin/nodes/{id}/token", m.issueToken)
		r.Post("/admin/nodes/{id}/revoke", m.revokeNode)
		r.Post("/admin/nodes/{id}/delete", m.deleteNode)
		r.Post("/admin/nodes/{id}/probe", m.probeNode)
		r.Get("/admin/connections", m.connectionsPage)
		r.Head("/admin/connections", m.connectionsPage)
		r.Post("/admin/connections/{id}/reveal", m.revealIP)
	})
}

func (m *AdminModule) now() time.Time {
	if m.d.Now == nil {
		return time.Now()
	}

	return m.d.Now()
}

func noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex")
		next.ServeHTTP(w, r)
	})
}

func (m *AdminModule) page(w http.ResponseWriter, r *http.Request, status int, title, section string, content, fragment templ.Component) {
	sections := layout.OperatorAdminSections
	if m.d.IsAdmin(r) {
		sections = layout.AdminSections
	}

	m.d.Render.Page(w, r, status, layout.Page{Title: title, Section: layout.SectionAdmin},
		layout.AdminPageWith(section, sections, content), fragment)
}

func (m *AdminModule) list(w http.ResponseWriter, r *http.Request) {
	devices, err := m.d.Devices.List(r.Context())
	if err != nil {
		m.d.Logger.ErrorContext(r.Context(), "list devices", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	m.page(w, r, http.StatusOK, "Devices", "devices", devicesPage(devices), devicesTable(devices))
}

// deviceView is the detail page of a device.
type deviceView struct {
	Device    *domain.Device
	NodeName  string
	CanAdmin  bool
	Failure   string
	Schedules []ScheduleRow
	// ActivePreset names the preset the node last switched the device to
	// (empty: none).
	ActivePreset string
	// SchedulesUnavailable is set when the schedules could not be read.
	SchedulesUnavailable bool
}

func (m *AdminModule) view(r *http.Request) (deviceView, int) {
	d, err := m.d.Devices.Get(r.Context(), chi.URLParam(r, "id"))

	switch {
	case errors.Is(err, domain.ErrDeviceNotFound):
		return deviceView{}, http.StatusNotFound
	case err != nil:
		m.d.Logger.ErrorContext(r.Context(), "get device", slog.Any("error", err))

		return deviceView{}, http.StatusInternalServerError
	}

	v := deviceView{Device: d, NodeName: d.Node().String(), CanAdmin: m.d.IsAdmin(r)}

	if id := d.ActivePreset(); !id.IsZero() {
		v.ActivePreset = id.String()

		if m.d.PresetName != nil {
			if name := m.d.PresetName(r.Context(), id); name != "" {
				v.ActivePreset = name
			}
		}
	}

	if n, err := m.d.Nodes.Get(r.Context(), d.Node().String()); err == nil {
		v.NodeName = n.Name().String()
	}

	if m.d.Schedules != nil {
		rows, err := m.d.Schedules.ForDevice(r.Context(), d.ID().String())
		if err != nil {
			m.d.Logger.WarnContext(r.Context(), "schedules of a device unavailable", slog.String("device_id", d.ID().String()),
				slog.Any("error", err))

			v.SchedulesUnavailable = true
		}

		v.Schedules = rows
	}

	return v, http.StatusOK
}

func (m *AdminModule) detail(w http.ResponseWriter, r *http.Request) {
	v, status := m.view(r)
	if status != http.StatusOK {
		m.d.Render.Error(w, r, status)

		return
	}

	m.page(w, r, http.StatusOK, "Device "+v.Device.Name(), "devices", devicePage(v), nil)
}

func (m *AdminModule) forget(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	err := m.d.Devices.Forget(r.Context(), audit.Caller, id)
	if err == nil {
		redirect(w, r, "/admin/devices")

		return
	}

	v, status := m.view(r)
	if status != http.StatusOK {
		m.d.Render.Error(w, r, status)

		return
	}

	status = http.StatusConflict
	v.Failure = "This device is still reported by its node: remove it from the node config first."

	if !errors.Is(err, domain.ErrDeviceStillReported) {
		m.d.Logger.ErrorContext(r.Context(), "forget device", slog.String("device_id", id), slog.Any("error", err))

		status, v.Failure = http.StatusInternalServerError, "The device could not be forgotten. Try again later."
	}

	m.page(w, r, status, "Device "+v.Device.Name(), "devices", devicePage(v), nil)
}

// disabledReason explains why the hub disabled a schedule.
func disabledReason(reason string) string {
	switch reason {
	case "device_stale":
		return "the node no longer reports this device"
	case "device_removed":
		return "the device was removed from the registry"
	case "preset_incompatible":
		return "its preset no longer fits this device"
	default:
		return reason
	}
}

// frequency formats a frequency in Hz for people.
func frequency(hz int64) string {
	switch {
	case hz >= 1_000_000:
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(float64(hz)/1e6, 'f', 6, 64), "0"), ".") + " MHz"
	case hz >= 1_000:
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(float64(hz)/1e3, 'f', 3, 64), "0"), ".") + " kHz"
	default:
		return strconv.FormatInt(hz, 10) + " Hz"
	}
}

func sampleRates(rates []int64) string {
	parts := make([]string, len(rates))
	for i, r := range rates {
		parts[i] = frequency(r)
	}

	return strings.Join(parts, ", ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

// listenPolicy shows a device's listen policy override and its origin:
// devices are configured only in their node config (ACC-013).
func listenPolicy(p string) string {
	if p == "" {
		return "global listen policy"
	}

	return p + " (node.toml)"
}

func utc(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

func missing(d *domain.Device) (string, bool) {
	since, ok := d.Missing()
	if !ok {
		return "", false
	}

	return utc(since), true
}

func state(d *domain.Device) string {
	st, _, reason := d.State()
	if reason != "" && reason != domain.ReasonNotReported {
		return string(st) + " (" + reason + ")"
	}

	return string(st)
}
