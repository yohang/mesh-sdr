package wire

import (
	"context"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/decodes"
	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/mapfeatures"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/settings"
)

// topicMap is the map topic of /api/ws (MAP-002).
var topicMap = events.MustTopic("map")

// mapDeps are the hub parts the map features module uses.
type mapDeps struct {
	adapter  *db.DB
	broker   events.Publisher
	policies *gridapp.ListenPolicies
	identity interface {
		Principal(ctx context.Context) identitydomain.Principal
	}
	features *gridapp.Features
	store    *settings.Store
	now      func() time.Time
	logger   *slog.Logger
}

// newMap builds the map features module (MAP-002, MAP-007, MAP-012,
// MAP-013): the projection of the decodes, its deltas on the map topic, the
// expiry job and GET /api/v1/map/features and /map/config, all filtered by
// the listen policies.
func newMap(d mapDeps) *mapfeatures.Module {
	logger := component(d.logger, "mapfeatures.module")
	store := d.store

	return mapfeatures.New(mapfeatures.Deps{
		DB: d.adapter,
		Settings: func() mapfeatures.Settings {
			s := store.Snapshot()

			return mapfeatures.Settings{
				PositionRetention:     time.Duration(s.Int("map.position_retention_s")) * time.Second,
				CallRetention:         time.Duration(s.Int("map.call_retention_s")) * time.Second,
				MaxCalls:              s.Int("map.max_calls"),
				IgnoreIndirectReports: s.Bool("map.ignore_indirect_reports"),
				PreferRecentReports:   s.Bool("map.prefer_recent_reports"),
			}
		},
		Published: mapChanged(d.broker, d.policies, logger),
		Visible:   mapDevices(d.features, d.identity),
		Config:    func() mapfeatures.ConfigSettings { return mapConfig(store.Snapshot()) },
		Now:       d.now, Logger: logger,
	})
}

// mapConfig reads the map configuration settings.
func mapConfig(s *settings.Snapshot) mapfeatures.ConfigSettings {
	c := mapfeatures.ConfigSettings{
		StationName: s.String("receiver.name"), BaseLayers: s.Strings("map.base_layers"),
		DefaultBaseLayer: s.String("map.default_base_layer"), PositionRetentionS: s.Int("map.position_retention_s"),
		MaxCalls: s.Int("map.max_calls"), CallRetentionS: s.Int("map.call_retention_s"),
		PreciseReceivers: s.Bool("map.precise_receivers"),
		Links: mapfeatures.Links{
			Callsign: s.String("links.callsign_url"), Vessel: s.String("links.vessel_url"), Flight: s.String("links.flight_url"),
			ModeS: s.String("links.modes_url"), Sonde: s.String("links.sonde_url"),
		},
	}

	if lat, lon, ok := s.Geo("receiver.gps"); ok {
		c.Station = &mapfeatures.LatLon{Lat: lat, Lon: lon}
	}

	return c
}

// mapDevices returns the enabled devices the visitor may listen to, with
// their node and position (MAP-007).
func mapDevices(features *gridapp.Features, id interface {
	Principal(ctx context.Context) identitydomain.Principal
},
) func(ctx context.Context) ([]mapfeatures.Device, error) {
	return func(ctx context.Context) ([]mapfeatures.Device, error) {
		summary, err := features.Summary(ctx)
		if err != nil {
			return nil, err
		}

		names := map[string]string{}
		for _, n := range summary.Nodes {
			names[n.ID.String()] = n.Name
		}

		anonymous := id.Principal(ctx).IsAnonymous()
		out := []mapfeatures.Device{}

		for _, dev := range summary.Devices {
			if !dev.CanListen(anonymous) {
				continue
			}

			d := mapfeatures.Device{ID: dev.ID.String(), Name: dev.Name, NodeID: dev.Node.String(), NodeName: names[dev.Node.String()]}
			if p := dev.Position; p != nil {
				d.Position = &mapfeatures.Position{Lat: p.Lat(), Lon: p.Lon(), Own: p.Own()}
			}

			out = append(out, d)
		}

		return out, nil
	}
}

// mapChanged publishes a committed map change on the map topic, to the
// viewers who may listen to the feature's device. A feature without a
// device, or listen policies that cannot be read, reach nobody (fail
// closed).
func mapChanged(b events.Publisher, policies *gridapp.ListenPolicies, logger *slog.Logger) func(context.Context, mapfeatures.Change) {
	return func(ctx context.Context, c mapfeatures.Change) {
		ev := events.Event{Topic: topicMap}

		var device string

		switch {
		case c.Upsert != nil:
			ev.Type, ev.Payload, device = rxv1.TypeMapFeatureUpsert.String(), mapfeatures.ViewOf(*c.Upsert), c.Upsert.DeviceID
		case c.Remove != nil:
			ev.Type, ev.Payload, device = rxv1.TypeMapFeatureRemove.String(), *c.Remove, c.Remove.DeviceID
		default:
			return
		}

		if device == "" {
			return
		}

		view, err := policies.View(ctx)
		if err != nil {
			logger.ErrorContext(ctx, "listen policies for map event", slog.Any("error", err))

			return
		}

		ev.Audience = func(v events.Viewer) bool { return view.CanListen(v.Anonymous(), device) }
		b.Publish(ctx, ev)
	}
}

// mapDecode is the projection input of a stored decoded message.
func mapDecode(m decodes.Message) mapfeatures.Decode {
	return mapfeatures.Decode{
		NodeID: m.NodeID, DeviceID: m.DeviceID, Mode: m.Mode, Schema: m.Schema, FreqHz: m.FreqHz, At: m.DecodedAt,
		Payload: m.Payload,
	}
}
