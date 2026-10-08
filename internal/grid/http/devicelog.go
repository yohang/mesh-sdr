package http

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Admin › Devices › {id} › Log (SRC-005, ADM-015, admins only): the last
// records the node of the device pushed over its control channel, rendered
// as plain text; the page appends the live records it receives on the hub
// events socket (topic device_log:device=<id>, staff only) with an ES
// module (static/js/device-log.js).

// LogTimeLayout formats the time of a device log record, on the page and in
// the live records.
const LogTimeLayout = "2006-01-02 15:04:05 UTC"

// DeviceLogRecords reads the device logs held by the hub.
type DeviceLogRecords interface {
	Records(id shared.DeviceID) []app.LogRecord
}

// deviceLogView is the log page of a device.
type deviceLogView struct {
	Device  *domain.Device
	Records []app.LogRecord
}

func (v deviceLogView) topic() string { return "device_log:device=" + v.Device.ID().String() }

func (v deviceLogView) path() string { return "/admin/devices/" + v.Device.ID().String() + "/log" }

// logTime formats the time of a record.
func logTime(r app.LogRecord) string { return r.Time.UTC().Format(LogTimeLayout) }

// logOrigin names the source of a record: the connector (with its stderr
// class) or the device lifecycle.
func logOrigin(r app.LogRecord) string {
	if r.Source == "device" {
		return "device"
	}

	if r.Class != "" {
		return r.Source + " " + r.Class
	}

	return r.Source
}

func (m *AdminModule) deviceLog(w http.ResponseWriter, r *http.Request) {
	d, err := m.d.Devices.Get(r.Context(), chi.URLParam(r, "id"))

	switch {
	case errors.Is(err, domain.ErrDeviceNotFound):
		m.d.Render.Error(w, r, http.StatusNotFound)

		return
	case err != nil:
		m.d.Logger.ErrorContext(r.Context(), "get device", slog.Any("error", err))
		m.d.Render.Error(w, r, http.StatusInternalServerError)

		return
	}

	v := deviceLogView{Device: d}
	if m.d.Logs != nil {
		v.Records = m.d.Logs.Records(d.ID())
	}

	m.page(w, r, http.StatusOK, "Log of "+d.Name(), "devices", deviceLogPage(v), deviceLogList(v))
}
