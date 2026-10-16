package wire

import (
	"context"
	"fmt"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/mapfeatures"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/settings"
	"github.com/yohang/mesh-sdr/internal/version"
)

// stationStatus builds the public status (API-003) from the settings, the
// feature summary and the presets.
type stationStatus struct {
	settings *settings.Store
	features *gridapp.Features
	presets  *presets.Service
}

// Status implements api.StatusSource: the enabled devices, without node
// address. A device is online in the admin sense (ready or busy:
// gridapp.DeviceFeatures.Status), not only while it runs
// (DeviceFeatures.Online).
func (s stationStatus) Status(ctx context.Context) (api.StationStatus, error) {
	snap := s.settings.Snapshot()
	st := api.StationStatus{
		Name: snap.String("receiver.name"), Location: snap.String("receiver.location"), Version: version.String(),
		AdminEmail: snap.String("receiver.admin_email"), AdminEmailPublic: snap.Bool("receiver.admin_email_public"),
		Devices: []api.StatusDeviceInfo{},
	}

	// SR-32: the same coarse position as the map receivers, unless
	// map.precise_receivers.
	if lat, lon, ok := snap.Geo("receiver.gps"); ok {
		st.Lat, st.Lon, _ = mapfeatures.Place(lat, lon, snap.Bool("map.precise_receivers"))
		st.HasPosition = true
	}

	if e, ok := snap.Get("receiver.altitude_m"); ok && e.Source() != settings.SourceDefault {
		alt := snap.Int("receiver.altitude_m")
		st.Altitude = &alt
	}

	summary, err := s.features.Summary(ctx)
	if err != nil {
		return api.StationStatus{}, fmt.Errorf("feature summary: %w", err)
	}

	for _, d := range summary.Devices {
		info := api.StatusDeviceInfo{
			ID: d.ID.String(), Name: d.Name, Type: d.Type, Listeners: d.Listeners,
			Online: d.Status == griddomain.DeviceReady || d.Status == griddomain.DeviceBusy,
		}

		if !d.ActivePreset.IsZero() {
			if p, err := s.presets.Get(ctx, d.ActivePreset.String()); err == nil {
				info.Preset = &api.StatusPresetInfo{Name: p.Name(), CenterFreq: p.CenterFreq(), SampRate: p.SampRate()}
			}
		}

		st.Devices = append(st.Devices, info)
	}

	return st, nil
}
