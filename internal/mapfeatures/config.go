package mapfeatures

import "slices"

// Layer is a base layer of the map (MAP-004): a keyless tile provider the
// browser loads directly.
type Layer struct {
	ID          string
	Name        string
	URL         string
	Subdomains  string
	Attribution string
	MaxZoom     int
}

const (
	esriTiles = "https://server.arcgisonline.com/ArcGIS/rest/services/"
	osmCredit = "© OpenStreetMap contributors"
)

// layers are the base layers the hub knows, by id (config.BaseLayers).
var layers = []Layer{
	{ID: "osm", Name: "OpenStreetMap", URL: "https://tile.openstreetmap.org/{z}/{x}/{y}.png", Attribution: osmCredit, MaxZoom: 19},
	{
		ID: "opentopomap", Name: "OpenTopoMap", URL: "https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png", Subdomains: "abc",
		Attribution: "Map data: " + osmCredit + ", SRTM | Map style: © OpenTopoMap (CC-BY-SA)", MaxZoom: 17,
	},
	{
		ID: "esri_world_imagery", Name: "Esri World Imagery", URL: esriTiles + "World_Imagery/MapServer/tile/{z}/{y}/{x}",
		Attribution: "Tiles © Esri — Source: Esri, i-cubed, USDA, USGS, AEX, GeoEye, Getmapping, Aerogrid, IGN, IGP, UPR-EGP, and the GIS User Community",
		MaxZoom:     19,
	},
	{
		ID: "esri_world_street_map", Name: "Esri World Street Map", URL: esriTiles + "World_Street_Map/MapServer/tile/{z}/{y}/{x}",
		Attribution: "Tiles © Esri — Source: Esri, DeLorme, NAVTEQ, USGS, Intermap, iPC, NRCAN, Esri Japan, METI, Esri China (Hong Kong), Esri (Thailand), TomTom, 2012",
		MaxZoom:     19,
	},
	{
		ID: "esri_world_topo_map", Name: "Esri World Topo Map", URL: esriTiles + "World_Topo_Map/MapServer/tile/{z}/{y}/{x}",
		Attribution: "Tiles © Esri — Esri, DeLorme, NAVTEQ, TomTom, Intermap, iPC, USGS, FAO, NPS, NRCAN, GeoBase, Kadaster NL, Ordnance Survey, Esri Japan, METI, Esri China (Hong Kong), and the GIS User Community",
		MaxZoom:     19,
	},
	{
		ID: "cartodb_positron", Name: "CARTO Positron", URL: "https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png",
		Subdomains: "abcd", Attribution: osmCredit + " © CARTO", MaxZoom: 20,
	},
	{
		ID: "cartodb_dark_matter", Name: "CARTO Dark Matter", URL: "https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png",
		Subdomains: "abcd", Attribution: osmCredit + " © CARTO", MaxZoom: 20,
	},
	{
		ID: "cartodb_voyager", Name: "CARTO Voyager", URL: "https://{s}.basemaps.cartocdn.com/rastertiles/voyager/{z}/{x}/{y}{r}.png",
		Subdomains: "abcd", Attribution: osmCredit + " © CARTO", MaxZoom: 20,
	},
}

// Layers returns the known base layers of ids, in the order of ids; unknown
// and repeated ids are skipped.
func Layers(ids []string) []Layer {
	out := []Layer{}

	for _, id := range ids {
		i := slices.IndexFunc(layers, func(l Layer) bool { return l.ID == id })
		if i >= 0 && !slices.ContainsFunc(out, func(l Layer) bool { return l.ID == id }) {
			out = append(out, layers[i])
		}
	}

	return out
}

// Links are the lookup link templates delivered to clients (MAP-015): one
// {} placeholder each, "" when disabled.
type Links struct {
	Callsign, Vessel, Flight, ModeS, Sonde string
}

// ConfigSettings are the settings of the map configuration.
type ConfigSettings struct {
	StationName string
	// Station is receiver.gps (nil: unset).
	Station            *LatLon
	BaseLayers         []string
	DefaultBaseLayer   string
	PositionRetentionS int
	MaxCalls           int
	CallRetentionS     int
	// PreciseReceivers shows the receivers at their exact position (SR-32).
	PreciseReceivers bool
	Links            Links
}

// Position is the position of a device: its own, or its node's.
type Position struct {
	Lat, Lon float64
	Own      bool
}

// Device is a device a visitor may listen to.
type Device struct {
	ID, Name         string
	NodeID, NodeName string
	// Position is nil when its node config sets none.
	Position *Position
}

// Receiver kinds.
const (
	KindReceiverNode    = "node"
	KindReceiverDevice  = "device"
	KindReceiverStation = "station"
)

// Receiver is a receiver marker (MAP-007): a node position with its devices,
// a device with a position of its own, or the station position with the
// devices of the nodes that set none.
type Receiver struct {
	Key, Kind, Name string
	Lat, Lon        float64
	Locator         string
	Precise         bool
	Devices         []ReceiverDevice
}

// ReceiverDevice is a device of a receiver marker.
type ReceiverDevice struct {
	ID, Name string
}

// Receivers groups the devices into receiver markers, in device order.
// Unless precise, a marker sits at the centre of its 4-character locator.
func Receivers(devices []Device, station *LatLon, stationName string, precise bool) []Receiver {
	out := []Receiver{}
	index := map[string]int{}

	add := func(key, kind, name string, lat, lon float64, d Device) {
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, marker(key, kind, name, lat, lon, precise))
		}

		out[i].Devices = append(out[i].Devices, ReceiverDevice{ID: d.ID, Name: d.Name})
	}

	for _, d := range devices {
		switch p := d.Position; {
		case p != nil && p.Own:
			add("receiver:device:"+d.ID, KindReceiverDevice, d.Name, p.Lat, p.Lon, d)
		case p != nil:
			name := d.NodeName
			if name == "" {
				name = d.NodeID
			}

			add("receiver:node:"+d.NodeID, KindReceiverNode, name, p.Lat, p.Lon, d)
		case station != nil:
			add("receiver:station", KindReceiverStation, stationName, station.Lat, station.Lon, d)
		}
	}

	return out
}

// marker places a receiver marker (Place).
func marker(key, kind, name string, lat, lon float64, precise bool) Receiver {
	r := Receiver{Key: key, Kind: kind, Name: name, Precise: precise, Devices: []ReceiverDevice{}}
	r.Lat, r.Lon, r.Locator = Place(lat, lon, precise)

	return r
}

// Place is the public position of a receiver or of the station (SR-32), the
// one rule of every public surface (map receivers, GET /api/v1/status):
// precise, the position itself and its 6-character locator; otherwise the
// centre of its 4-character locator (about 100 by 200 km) and that locator.
func Place(lat, lon float64, precise bool) (float64, float64, string) {
	if precise {
		return lat, lon, Locator(lat, lon, 6)
	}

	loc := Locator(lat, lon, 4)
	sq, _ := ParseLocator(loc)
	clat, clon := sq.Center()

	return clat, clon, loc
}
