// Package mapfeatures is the data side of the map (MAP-002, MAP-007,
// MAP-012, MAP-013, ADR 0029): the features the hub projects from the
// decoded messages at ingest (APRS positions and objects, WSJT and JS8
// locators and the call lines between located stations), stored in
// map_features in the ingest transaction and published as
// map.feature.upsert / map.feature.remove deltas after the commit; the
// expiry job; the receiver markers; and GET /api/v1/map/features and
// /api/v1/map/config. Every device has its own features (key
// <kind>:<subject>@<device>), with its own track, filters and call cap: a
// visitor sees the features of the devices they may listen to, and the map
// merges a subject heard by several devices.
package mapfeatures

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
)

// Kind is the kind of a feature (TECHNICAL_SPEC §7.1 map_features.kind).
type Kind string

// Feature kinds the hub writes so far.
const (
	KindAPRS    Kind = "aprs"
	KindLocator Kind = "locator"
	KindCall    Kind = "call"
)

// SourceDecode is the source of the features projected from decodes.
const SourceDecode = "decode"

// Geometry types.
const (
	GeometryPoint   = "point"
	GeometryLocator = "locator"
	GeometryLine    = "line"
)

// MaxTrack bounds the track of a moving station (TECHNICAL_SPEC §7.1).
const MaxTrack = 100

// LatLon is a position in decimal degrees (WGS 84).
type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Endpoint is one end of a call line: a located station.
type Endpoint struct {
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Callsign string  `json:"callsign"`
	Locator  string  `json:"locator"`
}

// Geometry is the shape of a feature: a point (with the track of a moving
// station), a locator square or a line between two stations.
type Geometry struct {
	Type    string    `json:"type"`
	Lat     *float64  `json:"lat,omitempty"`
	Lon     *float64  `json:"lon,omitempty"`
	Track   []LatLon  `json:"track,omitempty"`
	Locator string    `json:"locator,omitempty"`
	From    *Endpoint `json:"from,omitempty"`
	To      *Endpoint `json:"to,omitempty"`
}

// Feature is one map feature of one device. Details holds plain-text
// values only.
type Feature struct {
	// Key is `<kind>:<subject>@<device>` (KeyOf): every device has its own
	// features.
	Key  string
	Kind Kind
	// Subject identifies what the feature shows across devices: a
	// callsign, an object name, a call ("DL1ABC>F4ABC").
	Subject  string
	Source   string
	DeviceID string
	// Lat and Lon are nil for call lines.
	Lat, Lon  *float64
	Geometry  Geometry
	Details   map[string]any
	UpdatedAt time.Time
	// ExpiresAt is the absolute expiry; zero: permanent.
	ExpiresAt time.Time
}

// View is the JSON view of a feature: the map.feature.upsert payload and an
// item of GET /api/v1/map/features. Times are Unix milliseconds.
type View struct {
	Key       string         `json:"key"`
	Kind      string         `json:"kind"`
	Subject   string         `json:"subject"`
	Source    string         `json:"source"`
	DeviceID  string         `json:"device_id,omitempty"`
	Lat       *float64       `json:"lat,omitempty"`
	Lon       *float64       `json:"lon,omitempty"`
	Geometry  Geometry       `json:"geometry"`
	Details   map[string]any `json:"details"`
	UpdatedAt int64          `json:"updated_at"`
	ExpiresAt *int64         `json:"expires_at,omitempty"`
}

// ViewOf returns the view of a feature.
func ViewOf(f Feature) View {
	v := View{
		Key: f.Key, Kind: string(f.Kind), Subject: f.Subject, Source: f.Source, DeviceID: f.DeviceID, Lat: f.Lat, Lon: f.Lon,
		Geometry: f.Geometry, Details: f.Details, UpdatedAt: f.UpdatedAt.UnixMilli(),
	}

	if v.Details == nil {
		v.Details = map[string]any{}
	}

	if !f.ExpiresAt.IsZero() {
		ms := f.ExpiresAt.UnixMilli()
		v.ExpiresAt = &ms
	}

	return v
}

// KeyOf returns the key of the feature of a device: kind, subject and
// device.
func KeyOf(kind Kind, subject, device string) string {
	return string(kind) + ":" + subject + "@" + device
}

// MaxFeatures bounds GET /map/features: the newest features only.
const MaxFeatures = 5000

// Removal reasons of map.feature.remove.
const (
	ReasonExpired = "expired"
	ReasonDeleted = "deleted"
)

// Removal is a feature that left the map.
type Removal struct {
	Key      string `json:"key"`
	Reason   string `json:"reason"`
	DeviceID string `json:"-"`
}

// Change is a committed change of the map: an upsert or a removal.
type Change struct {
	Upsert *Feature
	Remove *Removal
}

// Settings are the map settings the module applies (MAP-011, MAP-012,
// MAP-013).
type Settings struct {
	PositionRetention     time.Duration
	CallRetention         time.Duration
	MaxCalls              int
	IgnoreIndirectReports bool
	PreferRecentReports   bool
}

// Deps are the dependencies of the module.
type Deps struct {
	DB *db.DB
	// Settings returns the current map settings.
	Settings func() Settings
	// Published, when set, is told every committed change (the hub
	// events map.feature.upsert and map.feature.remove).
	Published func(ctx context.Context, c Change)
	// Visible returns the devices the caller of ctx may listen to: the
	// features and receivers they see.
	Visible func(ctx context.Context) ([]Device, error)
	// Config returns the map configuration settings (layers, links,
	// station).
	Config func() ConfigSettings
	Now    func() time.Time
	Logger *slog.Logger
}

// Module is the map features module.
type Module struct {
	d    Deps
	repo *Repository

	mu      sync.Mutex
	pending map[string][]Change
}

// New returns the module.
func New(d Deps) *Module {
	if d.Now == nil {
		d.Now = time.Now
	}

	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}

	return &Module{d: d, repo: NewRepository(d.DB), pending: map[string][]Change{}}
}

// encodeJSON encodes v as a JSON string.
func encodeJSON(v any) (string, error) {
	b, err := json.Marshal(v)

	return string(b), err
}
