package mapfeatures

import (
	"encoding/json"
	"math"
	"slices"
	"testing"
	"time"
)

// aircraftAt is an aircraft.v1 decode of device vhf at t0 + offset.
func aircraftAt(offset time.Duration, text string, payload map[string]any) Decode {
	p, _ := json.Marshal(payload)
	d := dec(schemaAircraft, "adsb", string(p))
	d.At, d.Text, d.FreqHz = t0.Add(offset), text, 1_090_000_000

	return d
}

// An aircraft of a device is one feature keyed by its address: it appears
// with its first position, keeps the details of earlier records, its
// track and a capped log; the course comes from its last two positions
// when the record has none; its lifetime runs from its last position.
func TestAircraftFeature(t *testing.T) {
	e := newEnv(t)
	key := "aircraft:4CA1FA@vhf"

	e.ingest(t, aircraftAt(0, "4CA1FA EIN5AT", map[string]any{"icao": "4CA1FA", "flight": "EIN5AT", "category": "A3", "messages": 3}))

	if _, ok := e.get(t, key); ok || len(e.take()) != 0 {
		t.Fatal("an aircraft without a position is on the map")
	}

	e.ingest(t, aircraftAt(time.Second, "4CA1FA EIN5AT 12000 ft 51.9,3.8", map[string]any{
		"icao": "4CA1FA", "flight": "EIN5AT", "category": "A3", "lat": 51.9, "lon": 3.8, "altitude_ft": 12000,
	}))

	f, ok := e.get(t, key)
	if !ok || f.Kind != KindAircraft || f.Subject != "4CA1FA" || *f.Lat != 51.9 || !f.ExpiresAt.Equal(t0.Add(time.Second+15*time.Minute)) {
		t.Fatalf("feature %+v", f)
	}

	d := f.Details
	if d["label"] != "EIN5AT" || d["flight"] != "EIN5AT" || d["category"] != "A3" || d["country"] != "Ireland" || d["country_code"] != "IE" ||
		d["altitude_ft"] != 12000.0 || d["freq_hz"] != 1_090_000_000.0 || d["mode"] != "adsb" || d["registration"] != nil || d["course"] != nil {
		t.Errorf("details %v", d)
	}

	// North-east of the last position, no track in the record.
	e.ingest(t, aircraftAt(11*time.Second, "4CA1FA 12500 ft 52,3.9", map[string]any{"icao": "4CA1FA", "lat": 52.0, "lon": 3.9, "altitude_ft": 12500}))

	f, _ = e.get(t, key)
	if c, _ := f.Details["course"].(float64); math.Abs(c-31.6) > 0.5 || len(f.Geometry.Track) != 1 || f.Geometry.Track[0] != (LatLon{Lat: 51.9, Lon: 3.8}) {
		t.Errorf("course %v, track %v", f.Details["course"], f.Geometry.Track)
	}

	// No position: the last one stays with its lifetime; the squawk and
	// emergency are kept, the reported course wins.
	e.ingest(t, aircraftAt(20*time.Second, "4CA1FA squawk 7700", map[string]any{"icao": "4CA1FA", "squawk": "7700", "emergency": "general", "track_deg": 90.5}))

	f, _ = e.get(t, key)
	if *f.Lat != 52 || !f.ExpiresAt.Equal(t0.Add(11*time.Second+15*time.Minute)) || f.Details["squawk"] != "7700" ||
		f.Details["emergency"] != "general" || f.Details["course"] != 90.5 || f.Details["altitude_ft"] != nil {
		t.Errorf("after a record without position %+v", f)
	}

	logs := aircraftLog(f.Details["log"])
	if len(logs) != 3 || logs[2]["text"] != "4CA1FA squawk 7700" || logs[0]["mode"] != "adsb" || f.Details["flight"] != "EIN5AT" {
		t.Errorf("log %v", logs)
	}

	for i := range MaxAircraftLog + 5 {
		e.ingest(t, aircraftAt(time.Duration(30+i)*time.Second, "4CA1FA "+string(rune('A'+i)), map[string]any{"icao": "4CA1FA"}))
	}

	f, _ = e.get(t, key)
	if logs := aircraftLog(f.Details["log"]); len(logs) != MaxAircraftLog || logs[MaxAircraftLog-1]["text"] != "4CA1FA O" {
		t.Errorf("capped log %v", logs)
	}

	// A position older than the lifetime is not written; an older record
	// than the shown one is dropped.
	e.now = t0.Add(time.Hour)
	e.take()
	e.ingest(t, aircraftAt(time.Minute, "late", map[string]any{"icao": "3C6586", "lat": 52.1, "lon": 4.3}))
	e.ingest(t, aircraftAt(5*time.Second, "old", map[string]any{"icao": "4CA1FA", "lat": 50.0, "lon": 3.0}))

	if got := describe(e.take()); len(got) != 0 {
		t.Errorf("published %v", got)
	}
}

// Without an address, an aircraft joins the device's aircraft of the same
// tail, else flight; once a record carries the address, the aircraft
// moves under it with its log. The address gives the registration of the
// states with a pattern (A835AF: N628TS).
func TestAircraftMerge(t *testing.T) {
	e := newEnv(t)

	e.ingest(t, aircraftAt(0, "N628TS acars", map[string]any{"registration": "N628TS", "lat": 40.0, "lon": -74.0}))

	if got := describe(e.take()); !slices.Equal(got, []string{"upsert aircraft:N628TS@vhf"}) {
		t.Fatalf("published %v", got)
	}

	e.ingest(t, aircraftAt(time.Second, "N628TS flight", map[string]any{"registration": "N628TS", "flight": "TS628"}))

	if f, _ := e.get(t, "aircraft:N628TS@vhf"); f.Details["flight"] != "TS628" || f.Details["label"] != "TS628" {
		t.Errorf("tail record %+v", f.Details)
	}

	e.ingest(t, aircraftAt(2*time.Second, "A835AF 35000 ft", map[string]any{"icao": "A835AF", "lat": 40.1, "lon": -74.1, "altitude_ft": 35000}))

	if got := describe(e.take()); !slices.Equal(got, []string{"upsert aircraft:N628TS@vhf", "remove aircraft:N628TS@vhf deleted vhf", "upsert aircraft:A835AF@vhf"}) {
		t.Errorf("published %v", got)
	}

	f, ok := e.get(t, "aircraft:A835AF@vhf")
	if !ok || f.Details["registration"] != "N628TS" || f.Details["flight"] != "TS628" || f.Details["country_code"] != "US" ||
		len(aircraftLog(f.Details["log"])) != 3 || len(f.Geometry.Track) != 1 {
		t.Errorf("merged %+v", f)
	}

	// A record with the flight only joins the aircraft of that flight.
	e.ingest(t, aircraftAt(3*time.Second, "TS628 acars", map[string]any{"icao": "A835AF", "flight": "TS628"}))
	e.ingest(t, aircraftAt(4*time.Second, "TS628 position", map[string]any{"flight": "TS628", "lat": 40.2, "lon": -74.2}))

	if f, _ := e.get(t, "aircraft:A835AF@vhf"); *f.Lat != 40.2 {
		t.Errorf("flight record not merged: %+v", f)
	}

	if _, ok := e.get(t, "aircraft:TS628@vhf"); ok {
		t.Error("a flight feature next to its aircraft")
	}
}

// Untrusted values are validated: an unknown aircraft, junk text and
// out-of-range numbers are dropped.
func TestParseAircraft(t *testing.T) {
	if _, ok := parseAircraft(json.RawMessage(`{"icao":"zz","flight":"<b>","registration":"--"}`)); ok {
		t.Error("an aircraft without identity")
	}

	r, ok := parseAircraft(json.RawMessage(`{"icao":"~4CA1FA","flight":"ein5at ","squawk":"9999","emergency":"panic","category":"Z1",` +
		`"lat":91,"lon":4,"altitude_ft":1e9,"speed_kt":-1,"track_deg":361}`))
	if !ok || r.icao != "~4CA1FA" || r.flight != "EIN5AT" || r.lat != nil {
		t.Fatalf("report %+v", r)
	}

	if want := map[string]any{"icao": "~4CA1FA", "flight": "EIN5AT"}; len(r.details) != len(want) {
		t.Errorf("details %v", r.details)
	}
}

func TestSubject(t *testing.T) {
	for _, c := range []struct{ schema, payload, want string }{
		{schemaAircraft, `{"icao":"4CA1FA"}`, "4CA1FA"},
		{schemaAircraft, `{"icao":"bad"}`, ""},
		{schemaAPRS, `{"icao":"4CA1FA"}`, ""},
	} {
		if got := Subject(c.schema, json.RawMessage(c.payload)); got != c.want {
			t.Errorf("%s %s: %q", c.schema, c.payload, got)
		}
	}
}
