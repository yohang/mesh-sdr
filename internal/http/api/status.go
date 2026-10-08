package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/yohang/mesh-sdr/internal/http/problem"
)

// StationStatus is what the public status is built from.
type StationStatus struct {
	Name, Location string
	Lat, Lon       float64
	HasPosition    bool
	Version        string
	// AdminEmail is the configured contact e-mail; it is published only
	// when AdminEmailPublic is true.
	AdminEmail       string
	AdminEmailPublic bool
	Devices          []StatusDeviceInfo
}

// StatusDeviceInfo is one enabled device of the station.
type StatusDeviceInfo struct {
	ID, Name, Type string
	Online         bool
	Listeners      int
	Preset         *StatusPresetInfo
}

// StatusPresetInfo is the active preset of a device.
type StatusPresetInfo struct {
	Name                 string
	CenterFreq, SampRate int64
}

// StatusSource reads the station status across the nodes (settings, device
// registry, presence, presets).
type StatusSource interface {
	Status(ctx context.Context) (StationStatus, error)
}

// StatusHandlers serve GET /status (API-003) and its /status.json alias.
type StatusHandlers struct {
	source StatusSource
	logger *slog.Logger
}

// NewStatusHandlers returns the handlers.
func NewStatusHandlers(source StatusSource, logger *slog.Logger) StatusHandlers {
	return StatusHandlers{source: source, logger: logger}
}

// body builds the public document: the e-mail only when it is public.
func (h StatusHandlers) body(ctx context.Context) (Status, error) {
	st, err := h.source.Status(ctx)
	if err != nil {
		return Status{}, err
	}

	out := Status{Name: st.Name, Version: st.Version, DeviceCount: len(st.Devices), Devices: make([]StatusDevice, 0, len(st.Devices))}

	if st.Location != "" {
		out.Location = &st.Location
	}

	if st.HasPosition {
		out.Position = &StatusPosition{Lat: st.Lat, Lon: st.Lon}
	}

	if st.AdminEmailPublic && st.AdminEmail != "" {
		out.AdminEmail = &st.AdminEmail
	}

	for _, d := range st.Devices {
		sd := StatusDevice{Id: d.ID, Name: d.Name, Type: d.Type, Online: d.Online, Listeners: d.Listeners}

		if p := d.Preset; p != nil {
			sd.Preset = &StatusPreset{Name: p.Name, CenterFreq: p.CenterFreq, SampRate: p.SampRate}
		}

		out.Devices = append(out.Devices, sd)
	}

	return out, nil
}

// GetStatus implements StrictServerInterface.
func (h StatusHandlers) GetStatus(ctx context.Context, _ GetStatusRequestObject) (GetStatusResponseObject, error) {
	b, err := h.body(ctx)
	if err != nil {
		return nil, err
	}

	return GetStatus200JSONResponse(b), nil
}

// Alias is the handler of /status.json, mounted by the hub router: the
// same body as GET /api/v1/status.
func (h StatusHandlers) Alias() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := h.body(r.Context())
		if err != nil {
			problem.ErrorHandler(h.logger)(w, r, err)

			return
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(b); err != nil {
			h.logger.DebugContext(r.Context(), "write status", slog.Any("error", err))
		}
	})
}
