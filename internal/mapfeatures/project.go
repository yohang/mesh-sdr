package mapfeatures

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Payload schemas the projection reads (internal/radio/infra/decoder).
const (
	schemaAPRS = "aprs.v1"
	schemaWSJT = "wsjt.v1"
	schemaJS8  = "js8.v1"
)

// Bounds of the plain-text values of a feature.
const (
	maxSubject = 64
	maxComment = 256
	maxShort   = 32
)

// callsign is a callsign as the WSJT and JS8 decoders print it.
var callsign = regexp.MustCompile(`^[A-Z0-9/]{3,16}$`)

// Decode is a stored decoded message: the input of the projection. Its
// payload is untrusted node data.
type Decode struct {
	NodeID   string
	DeviceID string
	Mode     string
	Schema   string
	FreqHz   int64
	At       time.Time
	Payload  json.RawMessage
}

// report is a feature a decode reports.
type report struct {
	feature Feature
	// indirect: heard through a digipeater or relayed (MAP-013).
	indirect bool
}

// call is a call between two stations: a line once both are located.
type call struct {
	from, to string
}

// projection is what a decode reports to the map.
type projection struct {
	reports []report
	// kills are the keys of killed APRS objects and items.
	kills []string
	calls []call
}

// project projects a decode (one Project per mode family).
func project(d Decode, s Settings) projection {
	switch d.Schema {
	case schemaAPRS:
		return projectAPRS(d, s)
	case schemaWSJT:
		return projectWSJT(d, s)
	case schemaJS8:
		return projectJS8(d, s)
	default:
		return projection{}
	}
}

// aprsPayload is the part of the aprs.v1 payload the map reads.
type aprsPayload struct {
	Source    string             `json:"source"`
	Type      string             `json:"type"`
	Key       string             `json:"key"`
	Hops      []string           `json:"hops"`
	Lat       *float64           `json:"lat"`
	Lon       *float64           `json:"lon"`
	Ambiguity int                `json:"ambiguity"`
	Symbol    string             `json:"symbol"`
	Course    *float64           `json:"course"`
	Speed     *float64           `json:"speed_kmh"`
	Altitude  *float64           `json:"altitude_m"`
	Comment   string             `json:"comment"`
	Name      string             `json:"name"`
	Live      *bool              `json:"live"`
	Weather   map[string]float64 `json:"weather"`
	Forwarded *aprsPayload       `json:"forwarded"`
}

// projectAPRS reports the position of an APRS station, object or item
// (third-party traffic reports the forwarded packet), and the killed
// objects and items.
func projectAPRS(d Decode, s Settings) projection {
	var p aprsPayload
	if err := json.Unmarshal(d.Payload, &p); err != nil {
		return projection{}
	}

	hops := p.Hops

	if p.Type == "thirdparty" {
		if p.Forwarded == nil {
			return projection{}
		}

		p = *p.Forwarded
		hops = append(hops, p.Hops...)
	}

	subject := plain(p.Key, maxSubject)
	if strings.TrimSpace(subject) == "" {
		return projection{}
	}

	key := KeyOf(KindAPRS, subject, d.DeviceID)

	if (p.Type == "object" || p.Type == "item") && p.Live != nil && !*p.Live {
		return projection{kills: []string{key}}
	}

	if p.Lat == nil || p.Lon == nil || !validPosition(*p.Lat, *p.Lon) {
		return projection{}
	}

	lat, lon := *p.Lat, *p.Lon
	details := map[string]any{"label": subject, "mode": d.Mode, "aprs_type": plain(p.Type, maxShort)}

	setText(details, "source", plain(p.Source, maxSubject))
	setText(details, "symbol", plain(p.Symbol, 2))
	setText(details, "comment", plain(p.Comment, maxComment))
	setNumber(details, "course", p.Course)
	setNumber(details, "speed_kmh", p.Speed)
	setNumber(details, "altitude_m", p.Altitude)

	if d.FreqHz > 0 {
		details["freq_hz"] = d.FreqHz
	}

	if p.Ambiguity > 0 && p.Ambiguity <= 4 {
		details["ambiguity"] = p.Ambiguity
	}

	if len(hops) > 0 {
		clean := make([]string, 0, min(len(hops), 8))
		for _, h := range hops[:min(len(hops), 8)] {
			clean = append(clean, plain(h, maxShort))
		}

		details["hops"] = clean
	}

	if w := weather(p.Weather); len(w) > 0 {
		details["weather"] = w
	}

	f := Feature{
		Key: key, Kind: KindAPRS, Subject: subject, Source: SourceDecode, DeviceID: d.DeviceID, Lat: &lat, Lon: &lon,
		Geometry: Geometry{Type: GeometryPoint, Lat: &lat, Lon: &lon}, Details: details,
		UpdatedAt: d.At, ExpiresAt: d.At.Add(s.PositionRetention),
	}

	return projection{reports: []report{{feature: f, indirect: len(hops) > 0}}}
}

// weather keeps the finite values of a weather report, at most 16.
func weather(w map[string]float64) map[string]float64 {
	out := map[string]float64{}

	for k, v := range w {
		if len(out) == 16 {
			break
		}

		if len(k) <= maxShort && !math.IsNaN(v) && !math.IsInf(v, 0) && plain(k, maxShort) == k {
			out[k] = v
		}
	}

	return out
}

// wsjtPayload is the part of the wsjt.v1 payload the map reads.
type wsjtPayload struct {
	Msg      string `json:"msg"`
	Callsign string `json:"callsign"`
	Locator  string `json:"locator"`
	Callee   string `json:"callee"`
	DB       *int   `json:"db"`
	DBm      *int   `json:"dbm"`
}

// projectWSJT reports the locator of the sender of a WSJT message, and the
// call between the sender and the station it answers.
func projectWSJT(d Decode, s Settings) projection {
	var p wsjtPayload
	if err := json.Unmarshal(d.Payload, &p); err != nil || !callsign.MatchString(p.Callsign) {
		return projection{}
	}

	var out projection

	if f, ok := locatorFeature(d, s, p.Callsign, p.Locator); ok {
		if p.DB != nil {
			f.Details["db"] = *p.DB
		}

		if p.DBm != nil {
			f.Details["dbm"] = *p.DBm
		}

		out.reports = append(out.reports, report{feature: f})
	}

	callee := p.Callee
	if callee == "" && p.Locator != "" {
		callee = answered(p.Msg, p.Callsign)
	}

	if callsign.MatchString(callee) && callee != p.Callsign {
		out.calls = append(out.calls, call{from: p.Callsign, to: callee})
	}

	return out
}

// answered returns the station a "CALLEE SENDER GRID" message answers
// ("" for a CQ, QRZ or DE message).
func answered(msg, sender string) string {
	f := strings.Fields(msg)
	if len(f) < 3 || f[1] != sender {
		return ""
	}

	switch f[0] {
	case "CQ", "QRZ", "DE":
		return ""
	}

	return f[0]
}

// js8Payload is the part of the js8.v1 payload the map reads.
type js8Payload struct {
	Callsign string   `json:"callsign"`
	To       string   `json:"to"`
	Locator  string   `json:"locator"`
	DB       *float64 `json:"db"`
}

// projectJS8 reports the locator of a JS8 heartbeat or compound frame, and
// the call of a directed frame.
func projectJS8(d Decode, s Settings) projection {
	var p js8Payload
	if err := json.Unmarshal(d.Payload, &p); err != nil || !callsign.MatchString(p.Callsign) {
		return projection{}
	}

	var out projection

	if f, ok := locatorFeature(d, s, p.Callsign, p.Locator); ok {
		setNumber(f.Details, "db", p.DB)
		out.reports = append(out.reports, report{feature: f})
	}

	if callsign.MatchString(p.To) && p.To != p.Callsign {
		out.calls = append(out.calls, call{from: p.Callsign, to: p.To})
	}

	return out
}

// locatorFeature is the locator square of a station (MAP-009).
func locatorFeature(d Decode, s Settings, call, locator string) (Feature, bool) {
	if locator == "" {
		return Feature{}, false
	}

	sq, err := ParseLocator(locator)
	if err != nil || len(sq.Locator) < 4 {
		return Feature{}, false
	}

	lat, lon := sq.Center()
	details := map[string]any{"label": call, "callsign": call, "locator": sq.Locator, "mode": d.Mode}

	if d.FreqHz > 0 {
		details["freq_hz"] = d.FreqHz
	}

	return Feature{
		Key: KeyOf(KindLocator, call, d.DeviceID), Kind: KindLocator, Subject: call, Source: SourceDecode, DeviceID: d.DeviceID, Lat: &lat, Lon: &lon,
		Geometry: Geometry{Type: GeometryLocator, Locator: sq.Locator}, Details: details,
		UpdatedAt: d.At, ExpiresAt: d.At.Add(s.PositionRetention),
	}, true
}

// callSubject is the subject of the call line between two stations,
// whichever called.
func callSubject(a, b string) string {
	if b < a {
		a, b = b, a
	}

	return a + ">" + b
}

func validPosition(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsNaN(lon) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// plain keeps untrusted text plain: valid UTF-8, no control characters, at
// most n bytes.
func plain(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}

		return r
	}, s)

	return shared.Truncate(s, n)
}

func setText(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func setNumber(m map[string]any, k string, v *float64) {
	if v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) {
		m[k] = *v
	}
}
