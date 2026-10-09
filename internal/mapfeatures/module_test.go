package mapfeatures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/http/api"
)

type env struct {
	m         *Module
	settings  Settings
	config    ConfigSettings
	visible   []Device
	now       time.Time
	published []Change
}

func newEnv(t *testing.T) *env {
	t.Helper()

	e := &env{settings: testSettings, now: t0, visible: []Device{{ID: "vhf", Name: "VHF", NodeID: "n1"}}}
	e.m = New(Deps{
		DB:        dbtest.NewSQLite(t),
		Settings:  func() Settings { return e.settings },
		Published: func(_ context.Context, c Change) { e.published = append(e.published, c) },
		Visible:   func(context.Context) ([]Device, error) { return e.visible, nil },
		Config:    func() ConfigSettings { return e.config },
		Now:       func() time.Time { return e.now },
	})

	return e
}

// ingest projects decodes in one transaction and flushes, like the decode
// ingestion.
func (e *env) ingest(t *testing.T, ds ...Decode) {
	t.Helper()

	err := e.m.d.DB.WithinTx(context.Background(), func(ctx context.Context) error {
		for _, d := range ds {
			if err := e.m.Ingest(ctx, d); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	e.m.Flush(context.Background(), "n1")
}

func (e *env) take() []Change {
	out := e.published
	e.published = nil

	return out
}

func (e *env) get(t *testing.T, key string) (Feature, bool) {
	t.Helper()

	f, ok, err := e.m.repo.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}

	return f, ok
}

func aprsAt(at time.Time, key string, lat, lon float64, hops ...string) Decode {
	if hops == nil {
		hops = []string{}
	}

	h, _ := json.Marshal(hops)
	p, _ := json.Marshal(map[string]any{"type": "position", "key": key, "source": key, "lat": lat, "lon": lon, "hops": json.RawMessage(h)})
	d := dec(schemaAPRS, "aprs", string(p))
	d.At = at

	return d
}

func describe(cs []Change) []string {
	var out []string

	for _, c := range cs {
		switch {
		case c.Upsert != nil:
			out = append(out, "upsert "+c.Upsert.Key)
		case c.Remove != nil:
			out = append(out, "remove "+c.Remove.Key+" "+c.Remove.Reason+" "+c.Remove.DeviceID)
		}
	}

	return out
}

// Reports are written in the ingest transaction and published after the
// commit; a moving station keeps a track; a rolled-back batch publishes
// nothing.
func TestIngestPublishesAfterCommit(t *testing.T) {
	e := newEnv(t)

	err := e.m.d.DB.WithinTx(context.Background(), func(ctx context.Context) error {
		return e.m.Ingest(ctx, aprsAt(t0, "F4ABC-9", 50, 3))
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(e.published) != 0 {
		t.Fatal("published before Flush")
	}

	e.m.Flush(context.Background(), "n1")

	if got := describe(e.take()); !slices.Equal(got, []string{"upsert aprs:F4ABC-9@vhf"}) {
		t.Errorf("published %v", got)
	}

	e.ingest(t, aprsAt(t0.Add(time.Minute), "F4ABC-9", 50.1, 3), aprsAt(t0.Add(2*time.Minute), "F4ABC-9", 50.1, 3))

	f, ok := e.get(t, "aprs:F4ABC-9@vhf")
	if !ok || *f.Lat != 50.1 || len(f.Geometry.Track) != 1 || f.Geometry.Track[0] != (LatLon{Lat: 50, Lon: 3}) ||
		!f.ExpiresAt.Equal(t0.Add(2*time.Minute+2*time.Hour)) {
		t.Errorf("feature %+v", f)
	}

	e.take()

	// A rolled-back batch is discarded.
	_ = e.m.d.DB.WithinTx(context.Background(), func(ctx context.Context) error {
		if err := e.m.Ingest(ctx, aprsAt(t0.Add(3*time.Minute), "F5XYZ", 45, 5)); err != nil {
			return err
		}

		return errors.New("rollback")
	})
	e.m.Discard("n1")
	e.m.Flush(context.Background(), "n1")

	if _, ok := e.get(t, "aprs:F5XYZ@vhf"); ok || len(e.take()) != 0 {
		t.Error("rolled-back report kept or published")
	}
}

// The track keeps the last MaxTrack positions.
func TestTrackBounded(t *testing.T) {
	e := newEnv(t)

	for i := range MaxTrack + 5 {
		e.ingest(t, aprsAt(t0.Add(time.Duration(i)*time.Second), "F4ABC-9", float64(i)/100, 3))
	}

	f, _ := e.get(t, "aprs:F4ABC-9@vhf")
	if n := len(f.Geometry.Track); n != MaxTrack || f.Geometry.Track[n-1].Lat != float64(MaxTrack+3)/100 {
		t.Errorf("track of %d points, last %+v", n, f.Geometry.Track[n-1])
	}
}

// Every device has its own features: a station heard by two devices is two
// features of the same kind and subject, each with its own track, filters
// and call lines; a call line joins only the locators of its own device,
// and the call cap applies per device.
func TestFeaturesStayPerDevice(t *testing.T) {
	e := newEnv(t)
	e.settings.MaxCalls = 1

	other := func(d Decode) Decode {
		d.DeviceID = "hf"

		return d
	}

	e.ingest(t, aprsAt(t0, "F4ABC-9", 50, 3))
	e.ingest(t, other(aprsAt(t0.Add(time.Minute), "F4ABC-9", 50.1, 3)))
	// An older report on vhf is still newer than vhf's own row.
	e.ingest(t, aprsAt(t0.Add(30*time.Second), "F4ABC-9", 50.2, 3))

	mine, _ := e.get(t, "aprs:F4ABC-9@vhf")
	theirs, _ := e.get(t, "aprs:F4ABC-9@hf")

	if mine.DeviceID != "vhf" || *mine.Lat != 50.2 || len(mine.Geometry.Track) != 1 || mine.Subject != "F4ABC-9" ||
		theirs.DeviceID != "hf" || *theirs.Lat != 50.1 || len(theirs.Geometry.Track) != 0 || theirs.Subject != "F4ABC-9" {
		t.Errorf("features %+v %+v", mine, theirs)
	}

	if got := describe(e.take()); !slices.Equal(got, []string{"upsert aprs:F4ABC-9@vhf", "upsert aprs:F4ABC-9@hf", "upsert aprs:F4ABC-9@vhf"}) {
		t.Errorf("published %v", got)
	}

	wsjt := func(at time.Duration, payload string) Decode {
		d := dec(schemaWSJT, "ft8", payload)
		d.At = t0.Add(at)

		return d
	}

	e.ingest(t, other(wsjt(2*time.Minute, `{"msg":"CQ DL1ABC JO62","callsign":"DL1ABC","locator":"JO62"}`)),
		wsjt(2*time.Minute, `{"msg":"CQ K1ABC FN42","callsign":"K1ABC","locator":"FN42"}`),
		wsjt(2*time.Minute, `{"msg":"DL1ABC K1ABC RR73","callsign":"K1ABC","callee":"DL1ABC"}`))

	if _, ok := e.get(t, "call:DL1ABC>K1ABC@vhf"); ok {
		t.Error("call line joins a locator of another device")
	}

	// One call line on each device: the cap of one does not remove the
	// other's.
	e.ingest(t, other(wsjt(3*time.Minute, `{"msg":"CQ F4XYZ JN18","callsign":"F4XYZ","locator":"JN18"}`)),
		other(wsjt(3*time.Minute, `{"msg":"F4XYZ DL1ABC JO62","callsign":"DL1ABC","locator":"JO62"}`)),
		wsjt(3*time.Minute, `{"msg":"CQ F4XYZ JN18","callsign":"F4XYZ","locator":"JN18"}`),
		wsjt(4*time.Minute, `{"msg":"F4XYZ K1ABC FN42","callsign":"K1ABC","locator":"FN42"}`))

	for _, key := range []string{"call:DL1ABC>F4XYZ@hf", "call:F4XYZ>K1ABC@vhf"} {
		if _, ok := e.get(t, key); !ok {
			t.Errorf("%s missing", key)
		}
	}
}

// MAP-013: indirect reports are dropped when map.ignore_indirect_reports is
// set; with map.prefer_recent_reports, a report older than the stored one
// is dropped, otherwise the last one received wins. A report already
// expired is never written.
func TestReportFiltering(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ignoreIndirect bool
		preferRecent   bool
		want           []float64 // latitude stored after each report
	}{
		{"defaults", false, true, []float64{50, 50, 51, 51}},
		{"ignore indirect", true, true, []float64{50, 50, 50, 50}},
		{"last received wins", false, false, []float64{50, 49, 51, 51}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.settings.IgnoreIndirectReports, e.settings.PreferRecentReports = tc.ignoreIndirect, tc.preferRecent
			e.now = t0.Add(3 * time.Hour)

			reports := []Decode{
				aprsAt(t0.Add(2*time.Hour), "F4ABC-9", 50, 3),
				aprsAt(t0.Add(90*time.Minute), "F4ABC-9", 49, 3),             // older
				aprsAt(t0.Add(150*time.Minute), "F4ABC-9", 51, 3, "F1ZZZ-1"), // newer, digipeated
				aprsAt(t0.Add(30*time.Minute), "F4ABC-9", 52, 3),             // expired already
			}

			for i, d := range reports {
				e.ingest(t, d)

				f, ok := e.get(t, "aprs:F4ABC-9@vhf")
				if !ok || *f.Lat != tc.want[i] {
					t.Errorf("after report %d: %+v %v, want lat %g", i, f.Lat, ok, tc.want[i])
				}
			}
		})
	}
}

// A killed object or item leaves the map at once.
func TestKilledObject(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, dec(schemaAPRS, "aprs", `{"type":"object","key":"LEADER","live":true,"lat":45,"lon":5}`))
	e.take()

	e.ingest(t, dec(schemaAPRS, "aprs", `{"type":"object","key":"LEADER","live":false,"lat":45,"lon":5}`))

	if _, ok := e.get(t, "aprs:LEADER@vhf"); ok {
		t.Error("killed object kept")
	}

	if got := describe(e.take()); !slices.Equal(got, []string{"remove aprs:LEADER@vhf deleted vhf"}) {
		t.Errorf("published %v", got)
	}

	// Killing an unknown object publishes nothing.
	e.ingest(t, dec(schemaAPRS, "aprs", `{"type":"item","key":"GHOST","live":false}`))

	if len(e.take()) != 0 {
		t.Error("removal of an unknown object published")
	}
}

// MAP-011: a call between two located stations is a line, kept for
// map.call_retention_s; only the newest map.max_calls lines stay.
func TestCallLines(t *testing.T) {
	e := newEnv(t)
	e.settings.MaxCalls = 1

	wsjt := func(at time.Duration, payload string) Decode {
		d := dec(schemaWSJT, "ft8", payload)
		d.At = t0.Add(at)

		return d
	}

	// The callee is not located yet: no line.
	e.ingest(t, wsjt(0, `{"msg":"K1ABC DL1ABC JO62","callsign":"DL1ABC","locator":"JO62"}`))
	e.ingest(t, wsjt(15*time.Second, `{"msg":"CQ K1ABC FN42","callsign":"K1ABC","locator":"FN42"}`))
	e.ingest(t, wsjt(30*time.Second, `{"msg":"DL1ABC K1ABC RR73","callsign":"K1ABC","callee":"DL1ABC"}`))

	f, ok := e.get(t, "call:DL1ABC>K1ABC@vhf")
	if !ok || f.Kind != KindCall || f.Lat != nil || f.Geometry.Type != GeometryLine || f.Geometry.From.Callsign != "K1ABC" ||
		f.Geometry.To.Locator != "JO62" || f.Geometry.To.Lat != 52.5 || !f.ExpiresAt.Equal(t0.Add(30*time.Second+5*time.Minute)) {
		t.Fatalf("call %+v", f)
	}

	e.take()

	// A newer call pushes the oldest line out.
	e.ingest(t, wsjt(45*time.Second, `{"msg":"CQ F4ABC JN18","callsign":"F4ABC","locator":"JN18"}`),
		wsjt(60*time.Second, `{"msg":"F4ABC K1ABC FN42","callsign":"K1ABC","locator":"FN42"}`))

	if _, ok := e.get(t, "call:DL1ABC>K1ABC@vhf"); ok {
		t.Error("oldest call kept beyond max_calls")
	}

	if got := describe(e.take()); !slices.Equal(got, []string{
		"upsert locator:F4ABC@vhf", "upsert locator:K1ABC@vhf", "upsert call:F4ABC>K1ABC@vhf", "remove call:DL1ABC>K1ABC@vhf deleted vhf",
	}) {
		t.Errorf("published %v", got)
	}

	// max_calls 0: no line at all.
	e.settings.MaxCalls = 0
	e.ingest(t, wsjt(75*time.Second, `{"msg":"DL1ABC F4ABC RR73","callsign":"F4ABC","callee":"DL1ABC"}`))

	if _, ok := e.get(t, "call:DL1ABC>F4ABC@vhf"); ok {
		t.Error("call line with max_calls 0")
	}
}

// MAP-012: the expiry job deletes the expired features and publishes their
// removal with their device.
func TestExpire(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, aprsAt(t0, "OLD", 45, 5), aprsAt(t0.Add(time.Hour), "NEW", 46, 5))
	e.take()

	e.now = t0.Add(2*time.Hour + time.Second)

	n, err := e.m.Expire(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}

	if got := describe(e.take()); !slices.Equal(got, []string{"remove aprs:OLD@vhf expired vhf"}) {
		t.Errorf("published %v", got)
	}

	if _, ok := e.get(t, "aprs:NEW@vhf"); !ok {
		t.Error("live feature deleted")
	}
}

// GET /map/features lists the live features of the devices the caller may
// listen to, and nothing else.
func TestGetMapFeatures(t *testing.T) {
	e := newEnv(t)

	other := aprsAt(t0, "HIDDEN", 45, 5)
	other.DeviceID = "closed"

	e.ingest(t, aprsAt(t0, "F4ABC-9", 50, 3), other, aprsAt(t0.Add(-3*time.Hour+time.Minute), "STALE", 45, 5))
	e.now = t0.Add(time.Hour)

	res, err := e.m.GetMapFeatures(context.Background(), api.GetMapFeaturesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}

	list := res.(api.GetMapFeatures200JSONResponse).Features
	if res.(api.GetMapFeatures200JSONResponse).Truncated {
		t.Error("truncated")
	}

	if len(list) != 1 || list[0].Key != "aprs:F4ABC-9@vhf" || *list[0].DeviceId != "vhf" || *list[0].ExpiresAt != t0.Add(2*time.Hour).UnixMilli() ||
		string(list[0].Geometry) != `{"type":"point","lat":50,"lon":3}` {
		t.Fatalf("features %+v", list)
	}

	// The upsert event carries the same view.
	f, _ := e.get(t, "aprs:F4ABC-9@vhf")
	if a, b := asJSON(t, ViewOf(f)), asJSON(t, list[0]); !reflect.DeepEqual(a, b) {
		t.Errorf("event view %v\nAPI view %v", a, b)
	}

	e.visible = nil

	res, err = e.m.GetMapFeatures(context.Background(), api.GetMapFeaturesRequestObject{})
	if err != nil || len(res.(api.GetMapFeatures200JSONResponse).Features) != 0 {
		t.Errorf("features without a visible device: %+v %v", res, err)
	}

	e.m.d.Visible = func(context.Context) ([]Device, error) { return nil, errors.New("down") }

	if _, err := e.m.GetMapFeatures(context.Background(), api.GetMapFeaturesRequestObject{}); err == nil {
		t.Error("features served without the listen policies")
	}
}

func asJSON(t *testing.T, v any) any {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}

	return out
}

// GET /map/features lists the newest MaxFeatures features and says when
// older ones were left out.
func TestGetMapFeaturesTruncated(t *testing.T) {
	e := newEnv(t)

	err := e.m.d.DB.WithinTx(context.Background(), func(ctx context.Context) error {
		for i := range MaxFeatures + 1 {
			lat := 45.0
			f := Feature{
				Key: KeyOf(KindAPRS, fmt.Sprint("S", i), "vhf"), Kind: KindAPRS, Subject: fmt.Sprint("S", i), Source: SourceDecode,
				DeviceID: "vhf", Lat: &lat, Lon: &lat, Geometry: Geometry{Type: GeometryPoint}, Details: map[string]any{},
				UpdatedAt: t0.Add(time.Duration(i) * time.Millisecond), ExpiresAt: t0.Add(time.Hour),
			}

			if err := e.m.repo.Upsert(ctx, f); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := e.m.GetMapFeatures(context.Background(), api.GetMapFeaturesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}

	out := res.(api.GetMapFeatures200JSONResponse)
	if !out.Truncated || len(out.Features) != MaxFeatures || out.Features[0].Subject != fmt.Sprint("S", MaxFeatures) ||
		out.Features[MaxFeatures-1].Subject != "S1" {
		t.Errorf("truncated %v, %d features, first %s", out.Truncated, len(out.Features), out.Features[0].Subject)
	}
}

// MAP-007: receivers are grouped per node position, per device position of
// its own, and at the station position for the others; coarse by default
// (SR-32).
func TestReceivers(t *testing.T) {
	devices := []Device{
		{ID: "a", Name: "A", NodeID: "attic", NodeName: "Attic", Position: &Position{Lat: 50.63, Lon: 3.06}},
		{ID: "b", Name: "B", NodeID: "attic", NodeName: "Attic", Position: &Position{Lat: 50.63, Lon: 3.06}},
		{ID: "c", Name: "C", NodeID: "attic", NodeName: "Attic", Position: &Position{Lat: 48.85, Lon: 2.35, Own: true}},
		{ID: "d", Name: "D", NodeID: "roof"},
		{ID: "e", Name: "E", NodeID: "cellar"},
	}
	station := &LatLon{Lat: 45.76, Lon: 4.83}

	type marker struct {
		key, kind, name, locator string
		lat, lon                 float64
		devices                  []string
	}

	summary := func(rs []Receiver) []marker {
		var out []marker

		for _, r := range rs {
			m := marker{key: r.Key, kind: r.Kind, name: r.Name, locator: r.Locator, lat: r.Lat, lon: r.Lon}
			for _, d := range r.Devices {
				m.devices = append(m.devices, d.ID)
			}

			out = append(out, m)
		}

		return out
	}

	coarse := summary(Receivers(devices, station, "Station", false))
	want := []marker{
		{"receiver:node:attic", "node", "Attic", "JO10", 50.5, 3, []string{"a", "b"}},
		{"receiver:device:c", "device", "C", "JN18", 48.5, 3, []string{"c"}},
		{"receiver:station", "station", "Station", "JN25", 45.5, 5, []string{"d", "e"}},
	}

	if len(coarse) != len(want) {
		t.Fatalf("receivers %+v", coarse)
	}

	for i := range want {
		if coarse[i].key != want[i].key || coarse[i].kind != want[i].kind || coarse[i].name != want[i].name || coarse[i].locator != want[i].locator ||
			coarse[i].lat != want[i].lat || coarse[i].lon != want[i].lon || !slices.Equal(coarse[i].devices, want[i].devices) {
			t.Errorf("receiver %d = %+v, want %+v", i, coarse[i], want[i])
		}
	}

	precise := Receivers(devices, nil, "Station", true)
	if len(precise) != 2 || precise[0].Lat != 50.63 || precise[0].Locator != "JO10mp" || !precise[0].Precise {
		t.Errorf("precise receivers %+v", precise)
	}
}

// Every base layer the settings accept is known, and the config lists the
// offered ones in order, once.
func TestLayers(t *testing.T) {
	if got := Layers(config.BaseLayers); len(got) != len(config.BaseLayers) {
		t.Errorf("known layers %d of %d", len(got), len(config.BaseLayers))
	}

	for _, l := range layers {
		if l.Name == "" || l.Attribution == "" || l.MaxZoom == 0 || len(l.URL) < 9 || l.URL[:8] != "https://" {
			t.Errorf("layer %+v", l)
		}
	}

	got := Layers([]string{"esri_world_imagery", "cartodb_voyager", "osm", "esri_world_imagery"})
	if len(got) != 2 || got[0].ID != "esri_world_imagery" || got[1].ID != "osm" {
		t.Errorf("layers %+v", got)
	}

	if d := config.DefaultSettings().Map; !slices.Contains(d.BaseLayers, d.DefaultBaseLayer) {
		t.Errorf("default layer %s not offered", d.DefaultBaseLayer)
	}
}

// GET /map/config carries the layers, lifetimes, links and receivers.
func TestGetMapConfig(t *testing.T) {
	e := newEnv(t)
	e.config = ConfigSettings{
		StationName: "Lille", Station: &LatLon{Lat: 50.63, Lon: 3.06}, BaseLayers: []string{"osm", "opentopomap"}, DefaultBaseLayer: "osm",
		PositionRetentionS: 7200, MaxCalls: 5, CallRetentionS: 300, Links: Links{Callsign: "https://www.qrzcq.com/call/{}"},
	}

	res, err := e.m.GetMapConfig(context.Background(), api.GetMapConfigRequestObject{})
	if err != nil {
		t.Fatal(err)
	}

	c := res.(api.GetMapConfig200JSONResponse)
	if c.StationName != "Lille" || len(c.BaseLayers) != 2 || *c.BaseLayers[1].Subdomains != "abc" || c.DefaultBaseLayer != "osm" ||
		c.PositionRetentionS != 7200 || c.MaxCalls != 5 || c.CallRetentionS != 300 || *c.Links.CallsignUrl != "https://www.qrzcq.com/call/{}" ||
		c.Links.VesselUrl != nil || len(c.Receivers) != 1 || c.Receivers[0].Locator != "JO10" || c.Receivers[0].Precise ||
		len(c.Receivers[0].Devices) != 1 || c.Receivers[0].Devices[0].Id != "vhf" {
		t.Errorf("config %+v", c)
	}
}
