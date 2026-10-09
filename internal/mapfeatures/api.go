package mapfeatures

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yohang/mesh-sdr/internal/http/api"
)

var _ api.MapHandlers = (*Module)(nil)

// Features returns the newest MaxFeatures features the caller of ctx may
// see (those of the devices they may listen to, not expired), newest
// first, and whether older ones were left out.
func (m *Module) Features(ctx context.Context) ([]Feature, bool, error) {
	devices, err := m.d.Visible(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("visible devices: %w", err)
	}

	ids := make([]string, 0, len(devices))
	for _, d := range devices {
		ids = append(ids, d.ID)
	}

	list, err := m.repo.List(ctx, ids, m.d.Now(), MaxFeatures+1)
	if err != nil || len(list) <= MaxFeatures {
		return list, false, err
	}

	return list[:MaxFeatures], true, nil
}

// GetMapFeatures implements api.MapHandlers (GET /map/features).
func (m *Module) GetMapFeatures(ctx context.Context, _ api.GetMapFeaturesRequestObject) (api.GetMapFeaturesResponseObject, error) {
	list, truncated, err := m.Features(ctx)
	if err != nil {
		return nil, err
	}

	out := api.GetMapFeatures200JSONResponse{Features: make([]api.MapFeature, 0, len(list)), Truncated: truncated}

	for _, f := range list {
		v, err := apiFeature(f)
		if err != nil {
			return nil, err
		}

		out.Features = append(out.Features, v)
	}

	return out, nil
}

func apiFeature(f Feature) (api.MapFeature, error) {
	v := ViewOf(f)

	geometry, err := json.Marshal(v.Geometry)
	if err != nil {
		return api.MapFeature{}, err
	}

	details, err := json.Marshal(v.Details)
	if err != nil {
		return api.MapFeature{}, err
	}

	return api.MapFeature{
		Key: v.Key, Kind: api.MapFeatureKind(v.Kind), Subject: v.Subject, Source: api.MapFeatureSource(v.Source), DeviceId: optional(v.DeviceID),
		Lat: v.Lat, Lon: v.Lon, Geometry: geometry, Details: details, UpdatedAt: v.UpdatedAt, ExpiresAt: v.ExpiresAt,
	}, nil
}

// GetMapConfig implements api.MapHandlers (GET /map/config).
func (m *Module) GetMapConfig(ctx context.Context, _ api.GetMapConfigRequestObject) (api.GetMapConfigResponseObject, error) {
	devices, err := m.d.Visible(ctx)
	if err != nil {
		return nil, fmt.Errorf("visible devices: %w", err)
	}

	c := m.d.Config()
	out := api.GetMapConfig200JSONResponse{
		StationName: c.StationName, DefaultBaseLayer: c.DefaultBaseLayer, BaseLayers: []api.MapLayer{},
		PositionRetentionS: c.PositionRetentionS, MaxCalls: c.MaxCalls, CallRetentionS: c.CallRetentionS,
		Links: api.MapLinks{
			CallsignUrl: optional(c.Links.Callsign), VesselUrl: optional(c.Links.Vessel), FlightUrl: optional(c.Links.Flight),
			ModesUrl: optional(c.Links.ModeS), SondeUrl: optional(c.Links.Sonde),
		},
		Receivers: []api.MapReceiver{},
	}

	for _, l := range Layers(c.BaseLayers) {
		out.BaseLayers = append(out.BaseLayers, api.MapLayer{
			Id: l.ID, Name: l.Name, Url: l.URL, Subdomains: optional(l.Subdomains), Attribution: l.Attribution, MaxZoom: l.MaxZoom,
		})
	}

	for _, r := range Receivers(devices, c.Station, c.StationName, c.PreciseReceivers) {
		rv := api.MapReceiver{
			Key: r.Key, Kind: api.MapReceiverKind(r.Kind), Name: r.Name, Lat: r.Lat, Lon: r.Lon, Locator: r.Locator,
			Precise: r.Precise, Devices: make([]api.MapReceiverDevice, 0, len(r.Devices)),
		}

		for _, d := range r.Devices {
			rv.Devices = append(rv.Devices, api.MapReceiverDevice{Id: d.ID, Name: d.Name})
		}

		out.Receivers = append(out.Receivers, rv)
	}

	return out, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}
