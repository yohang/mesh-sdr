package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
)

// GeoPoint is an optional position in decimal degrees (WGS 84): a TOML
// inline table { lat = 50.63, lon = 3.06 }, JSON {"lat":…, "lon":…} (null
// when unset) or "<lat>,<lon>" in an env var.
type GeoPoint struct {
	set      bool
	lat, lon float64
}

// NewGeoPoint validates a position.
func NewGeoPoint(lat, lon float64) (GeoPoint, error) {
	if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return GeoPoint{}, fmt.Errorf("invalid position %g,%g: want latitude -90..90 and longitude -180..180", lat, lon)
	}

	return GeoPoint{set: true, lat: lat, lon: lon}, nil
}

// IsSet reports whether a position is set.
func (g GeoPoint) IsSet() bool { return g.set }

// Lat returns the latitude.
func (g GeoPoint) Lat() float64 { return g.lat }

// Lon returns the longitude.
func (g GeoPoint) Lon() float64 { return g.lon }

// UnmarshalText parses "<lat>,<lon>" (env vars).
func (g *GeoPoint) UnmarshalText(text []byte) error {
	a, b, ok := strings.Cut(string(text), ",")
	if !ok {
		return fmt.Errorf("invalid position %q: want <lat>,<lon>", text)
	}

	lat, err1 := strconv.ParseFloat(strings.TrimSpace(a), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(b), 64)

	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("invalid position %q: want <lat>,<lon>", text)
	}

	v, err := NewGeoPoint(lat, lon)
	if err != nil {
		return err
	}

	*g = v

	return nil
}

// UnmarshalTOML decodes { lat = …, lon = … }.
func (g *GeoPoint) UnmarshalTOML(data any) error {
	m, ok := data.(map[string]any)
	if !ok {
		return errors.New("invalid position: want { lat = <deg>, lon = <deg> }")
	}

	num := func(k string) (float64, bool) {
		switch v := m[k].(type) {
		case float64:
			return v, true
		case int64:
			return float64(v), true
		}

		return 0, false
	}

	lat, ok1 := num("lat")
	lon, ok2 := num("lon")

	if !ok1 || !ok2 || len(m) != 2 {
		return errors.New("invalid position: want { lat = <deg>, lon = <deg> }")
	}

	v, err := NewGeoPoint(lat, lon)
	if err != nil {
		return err
	}

	*g = v

	return nil
}

var errGeoShape = errors.New(`invalid position: want {"lat": <deg>, "lon": <deg>}`)

type geoJSON struct {
	Lat *float64 `json:"lat"`
	Lon *float64 `json:"lon"`
}

// UnmarshalJSON decodes {"lat":…, "lon":…} or null.
func (g *GeoPoint) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		*g = GeoPoint{}

		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()

	var v geoJSON
	if err := dec.Decode(&v); err != nil || v.Lat == nil || v.Lon == nil {
		return errGeoShape
	}

	p, err := NewGeoPoint(*v.Lat, *v.Lon)
	if err != nil {
		return err
	}

	*g = p

	return nil
}

// MarshalJSON encodes the position, or null when unset.
func (g GeoPoint) MarshalJSON() ([]byte, error) {
	if !g.set {
		return []byte("null"), nil
	}

	return json.Marshal(geoJSON{Lat: &g.lat, Lon: &g.lon})
}

// JSONSchema describes the type in the generated schema.
func (GeoPoint) JSONSchema() *jsonschema.Schema {
	props := jsonschema.NewProperties()
	props.Set("lat", &jsonschema.Schema{Type: "number", Minimum: json.Number("-90"), Maximum: json.Number("90"), Description: "Latitude in decimal degrees."})
	props.Set("lon", &jsonschema.Schema{Type: "number", Minimum: json.Number("-180"), Maximum: json.Number("180"), Description: "Longitude in decimal degrees."})

	return &jsonschema.Schema{
		Type: "object", Properties: props, Required: []string{"lat", "lon"},
		AdditionalProperties: jsonschema.FalseSchema,
	}
}

const ratePattern = `^[1-9][0-9]{0,4}/([0-9]+(ms|s|m|h|d|w))+$`

var rateRe = regexp.MustCompile(ratePattern)

// Rate is an event rate written "<count>/<window>": "5/1m" allows 5 events
// per minute.
type Rate struct {
	count  int
	window Duration
}

// ParseRate parses a rate.
func ParseRate(s string) (Rate, error) {
	if !rateRe.MatchString(s) {
		return Rate{}, fmt.Errorf("invalid rate %q: want <count>/<window> (for example \"5/1m\")", s)
	}

	c, w, _ := strings.Cut(s, "/")

	n, err := strconv.Atoi(c)
	if err != nil {
		return Rate{}, fmt.Errorf("invalid rate %q: %w", s, err)
	}

	d, err := ParseDuration(w)
	if err != nil {
		return Rate{}, fmt.Errorf("invalid rate %q: %w", s, err)
	}

	if d.Duration() < time.Second || d.Duration() > 24*time.Hour {
		return Rate{}, fmt.Errorf("invalid rate %q: the window must be 1s..24h", s)
	}

	return Rate{count: n, window: d}, nil
}

// MustRate is ParseRate that panics. Defaults and tests only.
func MustRate(s string) Rate {
	r, err := ParseRate(s)
	if err != nil {
		panic(err)
	}

	return r
}

// Count returns the number of events per window.
func (r Rate) Count() int { return r.count }

// Window returns the window.
func (r Rate) Window() time.Duration { return r.window.Duration() }

// String formats the rate.
func (r Rate) String() string { return strconv.Itoa(r.count) + "/" + r.window.String() }

// UnmarshalText implements encoding.TextUnmarshaler.
func (r *Rate) UnmarshalText(text []byte) error {
	v, err := ParseRate(string(text))
	if err != nil {
		return err
	}

	*r = v

	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (r Rate) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// JSONSchema describes the type in the generated schema.
func (Rate) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: ratePattern, Examples: []any{"5/1m", "20/1h"}}
}
