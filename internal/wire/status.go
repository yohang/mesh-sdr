package wire

import (
	"context"
	"fmt"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/settings"
	"github.com/yohang/mesh-sdr/internal/version"
)

// stationStatus builds the public status (API-003) from the settings, the
// device registry, the node links, the presence registry and the presets.
type stationStatus struct {
	settings *settings.Store
	devices  gridapp.DeviceLister
	links    gridapp.NodeLinks
	presence *gridapp.Presence
	presets  *presets.Service
}

// Status implements api.StatusSource. Disabled devices are left out; it
// carries no node address.
func (s stationStatus) Status(ctx context.Context) (api.StationStatus, error) {
	snap := s.settings.Snapshot()
	st := api.StationStatus{
		Name: snap.String("receiver.name"), Location: snap.String("receiver.location"), Version: version.String(),
		AdminEmail: snap.String("receiver.admin_email"), AdminEmailPublic: snap.Bool("receiver.admin_email_public"),
		Devices: []api.StatusDeviceInfo{},
	}

	st.Lat, st.Lon, st.HasPosition = snap.Geo("receiver.gps")

	devices, err := s.devices.List(ctx)
	if err != nil {
		return api.StationStatus{}, fmt.Errorf("list devices: %w", err)
	}

	conns, err := s.presence.List(ctx)
	if err != nil {
		return api.StationStatus{}, fmt.Errorf("list connections: %w", err)
	}

	listeners := map[string]int{}

	for _, c := range conns {
		if i := c.Info(); i.Kind == griddomain.ConnectionMedia && i.DeviceID != "" {
			listeners[i.DeviceID]++
		}
	}

	up := map[griddomain.NodeID]bool{}

	if s.links != nil {
		for _, id := range s.links.Connected() {
			up[id] = true
		}
	}

	for _, d := range devices {
		if !d.Flags().Enabled {
			continue
		}

		n := listeners[d.ID().String()]
		status := d.Status(up[d.Node()], n)

		info := api.StatusDeviceInfo{
			ID: d.ID().String(), Name: d.Name(), Type: d.Type(), Listeners: n,
			Online: status == griddomain.DeviceReady || status == griddomain.DeviceBusy,
		}

		if id := d.ActivePreset(); !id.IsZero() {
			if p, err := s.presets.Get(ctx, id.String()); err == nil {
				info.Preset = &api.StatusPresetInfo{Name: p.Name(), CenterFreq: p.CenterFreq(), SampRate: p.SampRate()}
			}
		}

		st.Devices = append(st.Devices, info)
	}

	return st, nil
}
