package mapfeatures

import (
	"cmp"
	"context"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Aircraft (AIR-006): the aircraft.v1 records of a device are merged into
// one feature per aircraft, keyed by its ICAO address, else its
// registration (tail), else its flight: a record without an address joins
// the device's aircraft of the same tail or flight, and an aircraft first
// known by its tail or flight moves under its address once a record
// carries both. The feature keeps the last position and its track, the
// course (computed from the last two positions when the record has none),
// the country and registration of its address (AIR-007) and a capped log
// of its messages. Its lifetime is the TTL of the mode from its last
// position (aircraft.adsb_ttl_s).

// schemaAircraft is the payload of the aircraft decoders
// (internal/radio/infra/decoder).
const schemaAircraft = "aircraft.v1"

// MaxAircraftLog bounds the message log of an aircraft.
const MaxAircraftLog = 10

// aircraftPayload is the part of the aircraft.v1 payload the map reads.
type aircraftPayload struct {
	ICAO         string   `json:"icao"`
	Registration string   `json:"registration"`
	Flight       string   `json:"flight"`
	Squawk       string   `json:"squawk"`
	Emergency    string   `json:"emergency"`
	Category     string   `json:"category"`
	Lat          *float64 `json:"lat"`
	Lon          *float64 `json:"lon"`
	AltitudeFt   *float64 `json:"altitude_ft"`
	Ground       bool     `json:"ground"`
	SpeedKt      *float64 `json:"speed_kt"`
	TrackDeg     *float64 `json:"track_deg"`
	VerticalRate *float64 `json:"vertical_rate_fpm"`
}

// Validation of the aircraft values (untrusted node data).
var (
	icaoAddress  = regexp.MustCompile(`^~?[0-9A-F]{6}$`)
	tailNumber   = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,9}$`)
	flightNumber = regexp.MustCompile(`^[A-Z0-9]{1,8}$`)
	squawkCode   = regexp.MustCompile(`^[0-7]{4}$`)
	categoryCode = regexp.MustCompile(`^[A-D][0-7]$`)
	emergencies  = []string{"general", "lifeguard", "minfuel", "nordo", "unlawful", "downed", "reserved"}
)

// aircraftReport is a validated aircraft.v1 record.
type aircraftReport struct {
	icao, tail, flight string
	lat, lon           *float64
	details            map[string]any
}

// parseAircraft validates an aircraft.v1 payload (ok false: nothing
// identifies the aircraft).
func parseAircraft(payload json.RawMessage) (aircraftReport, bool) {
	var p aircraftPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return aircraftReport{}, false
	}

	r := aircraftReport{details: map[string]any{}}

	if icaoAddress.MatchString(p.ICAO) {
		r.icao = p.ICAO
	}

	if t := strings.ToUpper(strings.TrimSpace(p.Registration)); tailNumber.MatchString(t) {
		r.tail = t
	}

	if f := strings.ToUpper(strings.TrimSpace(p.Flight)); flightNumber.MatchString(f) {
		r.flight = f
	}

	if r.icao == "" && r.tail == "" && r.flight == "" {
		return aircraftReport{}, false
	}

	if p.Lat != nil && p.Lon != nil && validPosition(*p.Lat, *p.Lon) {
		r.lat, r.lon = p.Lat, p.Lon
	}

	d := r.details
	setText(d, "icao", r.icao)
	setText(d, "registration", r.tail)
	setText(d, "flight", r.flight)

	if squawkCode.MatchString(p.Squawk) {
		d["squawk"] = p.Squawk
	}

	if slices.Contains(emergencies, p.Emergency) {
		d["emergency"] = p.Emergency
	}

	if categoryCode.MatchString(p.Category) {
		d["category"] = p.Category
	}

	if p.Ground {
		d["ground"] = true
	} else {
		setNumber(d, "altitude_ft", inRange(p.AltitudeFt, -2000, 100_000))
	}

	setNumber(d, "speed_kt", inRange(p.SpeedKt, 0, 5000))
	setNumber(d, "course", inRange(p.TrackDeg, 0, 360))
	setNumber(d, "vertical_rate_fpm", inRange(p.VerticalRate, -100_000, 100_000))

	if addr, ok := ParseICAO(r.icao); ok {
		if iso, name, ok := ICAOCountry(addr); ok {
			setText(d, "country", name)
			setText(d, "country_code", iso)
		}

		if r.tail == "" {
			if reg := ICAORegistration(addr); reg != "" {
				r.tail = reg
				d["registration"] = reg
			}
		}
	}

	return r, true
}

// inRange returns v when it is finite and within [lo, hi].
func inRange(v *float64, lo, hi float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < lo || *v > hi {
		return nil
	}

	return v
}

// ingestAircraft merges an aircraft.v1 record of a device into its
// aircraft feature.
func (m *Module) ingestAircraft(ctx context.Context, d Decode, s Settings, now time.Time) error {
	r, ok := parseAircraft(d.Payload)
	if !ok {
		return nil
	}

	var others []Feature

	subject := r.icao
	if subject == "" {
		// Without an address: the device's aircraft of the same tail, else
		// flight, else a new one keyed by them.
		var err error
		if others, err = m.repo.OfKind(ctx, d.DeviceID, KindAircraft); err != nil {
			return err
		}

		subject = matchAircraft(others, "registration", r.tail)
		if subject == "" {
			subject = matchAircraft(others, "flight", r.flight)
		}

		if subject == "" {
			subject = r.tail
		}

		if subject == "" {
			subject = r.flight
		}
	}

	key := KeyOf(KindAircraft, subject, d.DeviceID)

	prev, found, err := m.repo.Get(ctx, key)
	if err != nil {
		return err
	}

	if found && s.PreferRecentReports && prev.UpdatedAt.After(d.At) {
		return nil
	}

	// An aircraft known by its tail or flight only moves under its
	// address: its log comes along. Such an aircraft can only exist while
	// the addressed one lacks that tail or flight (else it would have
	// joined it): the device's aircraft are read only then, not on every
	// record of a known aircraft.
	var merged []Feature

	if r.icao != "" && ((r.tail != "" && prev.Details["registration"] != r.tail) || (r.flight != "" && prev.Details["flight"] != r.flight)) {
		if others, err = m.repo.OfKind(ctx, d.DeviceID, KindAircraft); err != nil {
			return err
		}

		for _, o := range others {
			if o.Key != key && o.Details["icao"] == nil &&
				((r.tail != "" && o.Details["registration"] == r.tail) || (r.flight != "" && o.Details["flight"] == r.flight)) {
				merged = append(merged, o)
			}
		}
	}

	// The first of them is the aircraft's previous state, the others give
	// their log.
	if !found && len(merged) > 0 {
		prev, found = merged[0], true
	}

	f, ok := m.aircraftFeature(d, s, r, subject, key, prev, found, merged)
	if !ok {
		return nil
	}

	for _, o := range merged {
		if err := m.remove(ctx, d.NodeID, o.Key, ReasonDeleted); err != nil {
			return err
		}
	}

	if !f.ExpiresAt.After(now) {
		return nil
	}

	return m.upsert(ctx, d.NodeID, f)
}

// matchAircraft returns the subject of the aircraft whose detail k is v.
func matchAircraft(features []Feature, k, v string) string {
	if v == "" {
		return ""
	}

	for _, f := range features {
		if f.Details[k] == v {
			return f.Subject
		}
	}

	return ""
}

// aircraftFeature builds the feature of an aircraft from its previous
// state and a record; ok is false when it has no position yet.
func (m *Module) aircraftFeature(d Decode, s Settings, r aircraftReport, subject, key string, prev Feature, found bool, merged []Feature) (Feature, bool) {
	details := map[string]any{}

	// The previous state is kept for what the record does not say (a
	// squawk heard once, the flight of an earlier message).
	if found {
		for k, v := range prev.Details {
			if k != "log" {
				details[k] = v
			}
		}

		// Ground and altitude, and the emergency, follow the last record.
		delete(details, "ground")
		delete(details, "altitude_ft")
		delete(details, "emergency")
	}

	for k, v := range r.details {
		details[k] = v
	}

	label := subject
	for _, k := range []string{"flight", "registration"} {
		if v, ok := details[k].(string); ok && v != "" {
			label = v

			break
		}
	}

	details["label"] = label
	details["mode"] = d.Mode

	if d.FreqHz > 0 {
		details["freq_hz"] = d.FreqHz
	}

	logs := aircraftLog(prev.Details["log"])
	for _, o := range merged {
		if o.Key != prev.Key {
			logs = append(logs, aircraftLog(o.Details["log"])...)
		}
	}

	slices.SortStableFunc(logs, func(a, b map[string]any) int {
		at, _ := a["at"].(float64)
		bt, _ := b["at"].(float64)

		return cmp.Compare(at, bt)
	})

	if t := plain(d.Text, maxComment); t != "" && (len(logs) == 0 || logs[len(logs)-1]["text"] != t) {
		logs = append(logs, map[string]any{"at": d.At.UnixMilli(), "mode": d.Mode, "text": t})
	}

	if len(logs) > MaxAircraftLog {
		logs = logs[len(logs)-MaxAircraftLog:]
	}

	details["log"] = logs

	f := Feature{
		Key: key, Kind: KindAircraft, Subject: subject, Source: SourceDecode, DeviceID: d.DeviceID,
		Details: details, UpdatedAt: d.At,
	}

	switch {
	case r.lat != nil:
		lat, lon := *r.lat, *r.lon
		f.Lat, f.Lon = &lat, &lon
		f.Geometry = Geometry{Type: GeometryPoint, Lat: &lat, Lon: &lon}
		f.ExpiresAt = d.At.Add(s.AircraftTTL)

		if found {
			f.Geometry.Track = track(prev, f)

			if _, ok := r.details["course"]; !ok && prev.Lat != nil && prev.Lon != nil && (*prev.Lat != lat || *prev.Lon != lon) {
				details["course"] = math.Round(bearing(*prev.Lat, *prev.Lon, lat, lon)*10) / 10
			}
		}
	case found && prev.Lat != nil:
		// No position in this record: the last one stays, with its
		// lifetime.
		f.Lat, f.Lon, f.Geometry, f.ExpiresAt = prev.Lat, prev.Lon, prev.Geometry, prev.ExpiresAt
	default:
		return Feature{}, false
	}

	return f, true
}

// aircraftLog reads a stored message log.
func aircraftLog(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items)+1)

	for _, it := range items {
		if e, ok := it.(map[string]any); ok {
			out = append(out, e)
		}
	}

	return out
}

// bearing returns the initial great-circle bearing from one position to
// another, in degrees from north (0 to 360).
func bearing(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dl := (lon2 - lon1) * math.Pi / 180

	y := math.Sin(dl) * math.Cos(p2)
	x := math.Cos(p1)*math.Sin(p2) - math.Sin(p1)*math.Cos(p2)*math.Cos(dl)

	return math.Mod(math.Atan2(y, x)*180/math.Pi+360, 360)
}

// Subject returns the subject a stored message is thinned by (TECHNICAL_SPEC
// §7.1 "Volume control"): the aircraft address of an aircraft.v1 record,
// else "".
func Subject(schema string, payload json.RawMessage) string {
	if schema != schemaAircraft {
		return ""
	}

	var p struct {
		ICAO string `json:"icao"`
	}

	if json.Unmarshal(payload, &p) != nil || !icaoAddress.MatchString(p.ICAO) {
		return ""
	}

	return p.ICAO
}
