package wire

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/http/api"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/mapfeatures"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

type principalKey struct{}

// ctxPrincipal is the identity of the principal the context carries
// (anonymous by default).
type ctxPrincipal struct{}

func (ctxPrincipal) Principal(ctx context.Context) identitydomain.Principal {
	if p, ok := ctx.Value(principalKey{}).(identitydomain.Principal); ok {
		return p
	}

	return identitydomain.Anonymous()
}

// mapSink records the map events a subscriber receives.
type mapSink struct {
	mu  sync.Mutex
	got []string
}

func (s *mapSink) receive(ev events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch p := ev.Payload.(type) {
	case mapfeatures.View:
		s.got = append(s.got, ev.Type+" "+p.Key)
	case mapfeatures.Removal:
		s.got = append(s.got, ev.Type+" "+p.Key)
	}
}

func (s *mapSink) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := s.got
	s.got = nil

	return out
}

// failingLister is a device registry that cannot be read.
type failingLister struct{}

func (failingLister) List(context.Context) ([]*domain.Device, error) {
	return nil, errors.New("db down")
}

// TestMapFollowsListenPolicies: an anonymous visitor gets neither the
// features (GET /api/v1/map/features) nor the map events (/api/ws topic
// map) of a device whose listen policy is registered; a signed-in listener
// gets both; listen policies that cannot be read publish nothing (fail
// closed). The receivers of GET /map/config follow the same rule.
func TestMapFollowsListenPolicies(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	now := time.Now()

	nodes, repo := gridsqlite.NewNodeRepository(a), gridsqlite.NewDeviceRepository(a)
	if err := nodes.Create(ctx, domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://attic:8074"), now)); err != nil {
		t.Fatal(err)
	}

	for id, policy := range map[string]string{"open": domain.ListenAnonymous, "closed": domain.ListenRegistered} {
		pos, err := domain.NewPosition(50.63, 3.06, false)
		if err != nil {
			t.Fatal(err)
		}

		d, err := domain.NewReportedDevice(domain.MustNodeID("attic"), domain.DeviceSpec{
			ID: shared.MustDeviceID(id), Name: id, Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2,
			SampleRates: []int64{1}, ListenPolicy: policy, Position: &pos,
		}, 0, now)
		if err != nil {
			t.Fatal(err)
		}

		if err := repo.Save(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	listen := gridapp.NewListenPolicies(repo, listenGlobal{v: domain.ListenRegistered}, quiet)
	features := gridapp.NewFeatures(gridapp.FeaturesDeps{
		Devices: repo, Caps: gridsqlite.NewCapabilityRepository(a), Listen: listen, Nodes: nodes,
	})
	broker := events.NewBroker()
	clock := now

	m := mapfeatures.New(mapfeatures.Deps{
		DB: a,
		Settings: func() mapfeatures.Settings {
			return mapfeatures.Settings{PositionRetention: time.Hour, CallRetention: time.Minute, MaxCalls: 5, PreferRecentReports: true}
		},
		Published: mapChanged(broker, listen, quiet),
		Visible:   mapDevices(features, ctxPrincipal{}),
		Config:    func() mapfeatures.ConfigSettings { return mapfeatures.ConfigSettings{BaseLayers: config.BaseLayers} },
		Now:       func() time.Time { return clock },
		Logger:    quiet,
	})

	listener := signedInListener(t)
	callers := map[string]context.Context{
		"anonymous": ctx,
		"listener":  context.WithValue(ctx, principalKey{}, listener),
	}
	sinks := map[string]*mapSink{}

	for name, cctx := range callers {
		sink := &mapSink{}
		sinks[name] = sink

		v := events.Viewer{}
		if name == "listener" {
			v.UserID = "u1"
		}

		sub := broker.Attach(v, topicAuthz{id: listenCaller{p: ctxPrincipal{}.Principal(cctx)}, policies: listen}, sink.receive)
		if _, _, err := sub.Subscribe(cctx, []string{"map"}); err != nil {
			t.Fatalf("%s subscribes to map: %v", name, err)
		}
	}

	aprs := func(device, call string) mapfeatures.Decode {
		p, _ := json.Marshal(map[string]any{"type": "position", "key": call, "lat": 50.6, "lon": 3.06, "hops": []string{}})

		return mapfeatures.Decode{NodeID: "attic", DeviceID: device, Mode: "aprs", Schema: "aprs.v1", At: now, Payload: p}
	}

	err := a.WithinTx(ctx, func(ctx context.Context) error {
		for _, d := range []mapfeatures.Decode{aprs("open", "F4OPEN"), aprs("closed", "F4CLOSED")} {
			if err := m.Ingest(ctx, d); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	m.Flush(ctx, "attic")

	want := map[string][]string{
		"anonymous": {"map.feature.upsert aprs:F4OPEN"},
		"listener":  {"map.feature.upsert aprs:F4OPEN", "map.feature.upsert aprs:F4CLOSED"},
	}

	for name, cctx := range callers {
		if got := sinks[name].take(); !slices.Equal(got, want[name]) {
			t.Errorf("%s events %v, want %v", name, got, want[name])
		}

		res, err := m.GetMapFeatures(cctx, api.GetMapFeaturesRequestObject{})
		if err != nil {
			t.Fatal(err)
		}

		var keys []string
		for _, f := range res.(api.GetMapFeatures200JSONResponse).Features {
			keys = append(keys, "map.feature.upsert "+f.Key)
		}

		slices.Sort(keys)

		if w := slices.Sorted(slices.Values(want[name])); !slices.Equal(keys, w) {
			t.Errorf("%s REST features %v, want %v", name, keys, w)
		}

		cfg, err := m.GetMapConfig(cctx, api.GetMapConfigRequestObject{})
		if err != nil {
			t.Fatal(err)
		}

		receivers := cfg.(api.GetMapConfig200JSONResponse).Receivers
		if n := len(want[name]); len(receivers) != 1 || len(receivers[0].Devices) != n || receivers[0].Name != "Attic" {
			t.Errorf("%s receivers %+v", name, receivers)
		}
	}

	// Removals follow the same audience.
	clock = now.Add(2 * time.Hour)

	if _, err := m.Expire(ctx); err != nil {
		t.Fatal(err)
	}

	if got := sinks["anonymous"].take(); !slices.Equal(got, []string{"map.feature.remove aprs:F4OPEN"}) {
		t.Errorf("anonymous removals %v", got)
	}

	if got := sinks["listener"].take(); len(got) != 2 {
		t.Errorf("listener removals %v", got)
	}

	// Listen policies that cannot be read publish nothing.
	rec := &recordedEvents{}
	broken := gridapp.NewListenPolicies(failingLister{}, listenGlobal{v: domain.ListenAnonymous}, quiet)
	f := mapfeatures.Feature{Key: "aprs:X", Kind: mapfeatures.KindAPRS, DeviceID: "open"}

	mapChanged(rec, broken, quiet)(ctx, mapfeatures.Change{Upsert: &f})
	mapChanged(rec, listen, quiet)(ctx, mapfeatures.Change{Remove: &mapfeatures.Removal{Key: "aprs:Y"}}) // no device

	if got := rec.take(); len(got) != 0 {
		t.Errorf("published without a listen view or a device: %+v", got)
	}
}

// The node reports each device's position: its own, else the node's
// (MAP-007).
func TestDevicesOfPositions(t *testing.T) {
	gps := func(lat, lon float64) config.GeoPoint {
		g, err := config.NewGeoPoint(lat, lon)
		if err != nil {
			t.Fatal(err)
		}

		return g
	}

	device := config.DeviceConfig{Name: "HF", Type: "rtl_sdr", SampleRates: []int64{1}}
	own := device
	own.GPS = gps(48.85, 2.35)

	cfg := config.Node{Devices: map[string]config.DeviceConfig{"a": device, "b": own}}

	if got := DevicesOf(cfg); got[0].GPS != nil || *got[1].GPS != (ctl.Position{Lat: 48.85, Lon: 2.35, Source: ctl.PositionDevice}) {
		t.Errorf("without node gps: %+v %+v", got[0].GPS, got[1].GPS)
	}

	cfg.Node.GPS = gps(50.63, 3.06)

	if got := DevicesOf(cfg); *got[0].GPS != (ctl.Position{Lat: 50.63, Lon: 3.06, Source: ctl.PositionNode}) || got[1].GPS.Source != ctl.PositionDevice {
		t.Errorf("with node gps: %+v %+v", got[0].GPS, got[1].GPS)
	}
}
