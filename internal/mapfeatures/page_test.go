package mapfeatures

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

type shell struct{}

func (shell) Shell(*http.Request) layout.Shell { return layout.Shell{SiteName: "Test"} }

// The Map page renders in the Map section with the island, its topic and
// initial state, the toolbar controls, the UTC clock and the list view.
func TestMapPage(t *testing.T) {
	e := newEnv(t)
	e.m.d.Render = render.New(shell{}, nil, slog.New(slog.DiscardHandler))
	signedIn := false
	e.m.d.SignedIn = func(context.Context) bool { return signedIn }

	r := chi.NewRouter()
	e.m.Routes(r)

	get := func(headers map[string]string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, Path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		return rec.Code, rec.Body.String()
	}

	code, body := get(nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", Path, code)
	}

	for _, want := range []string{
		"<title>Map · Test</title>", `<msdr-map class="flex flex-col gap-2" data-msdr-topics="map">`,
		`<script id="msdr-map-config" type="application/json"`, `{"signed_in":false,`,
		`<h1 class="mr-auto text-2xl font-semibold">Map</h1>`, `aria-controls="map-layers" data-map-toggle="layers"`,
		`aria-controls="map-legend" data-map-toggle="legend"`, `aria-pressed="false" data-map-list-toggle`,
		`<msdr-utc-clock`, `<time datetime="2026-10-08T12:00Z">12:00</time> UTC`, `role="region" aria-label="Map"`,
		`data-map-list hidden`, `<aside id="map-detail"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}

	if strings.Count(body, "<h1") != 1 {
		t.Errorf("%d h1", strings.Count(body, "<h1"))
	}

	signedIn = true
	if _, body := get(nil); !strings.Contains(body, `{"signed_in":true,`) {
		t.Error("signed-in visitor not told")
	}

	// A boosted navigation gets the full page (htmx keeps #main).
	if _, boosted := get(map[string]string{"HX-Request": "true", "HX-Boosted": "true"}); !strings.Contains(boosted, `<main id="main"`) {
		t.Error("boosted navigation did not get the full page")
	}
}

// The tile origins of the offered layers, one per subdomain, feed the CSP.
func TestTileOrigins(t *testing.T) {
	got := TileOrigins([]string{"osm", "opentopomap", "esri_world_imagery", "esri_world_topo_map", "cartodb_positron", "nope"})
	want := []string{
		"https://tile.openstreetmap.org",
		"https://a.tile.opentopomap.org", "https://b.tile.opentopomap.org", "https://c.tile.opentopomap.org",
		"https://server.arcgisonline.com",
		"https://a.basemaps.cartocdn.com", "https://b.basemaps.cartocdn.com", "https://c.basemaps.cartocdn.com", "https://d.basemaps.cartocdn.com",
	}

	if !slices.Equal(got, want) {
		t.Errorf("TileOrigins = %v, want %v", got, want)
	}

	e := newEnv(t)
	e.config = ConfigSettings{BaseLayers: []string{"osm"}}

	if got := e.m.ImageSources(); !slices.Equal(got, []string{"https://tile.openstreetmap.org"}) {
		t.Errorf("ImageSources = %v", got)
	}

	if got := TileOrigins(nil); len(got) != 0 {
		t.Errorf("no layers: %v", got)
	}
}

// A decode that locates a station links to it on the map; others do not.
func TestLink(t *testing.T) {
	tests := []struct {
		d    Decode
		want string
	}{
		{aprsAt(t0, "F4ABC-9", 50, 3), "/map?callsign=F4ABC-9"},
		{dec(schemaWSJT, "ft8", `{"msg":"CQ DL1ABC JO62","callsign":"DL1ABC","locator":"JO62"}`), "/map?callsign=DL1ABC"},
		{dec(schemaWSJT, "ft8", `{"msg":"DL1ABC F4ABC -10","callsign":"F4ABC"}`), ""},
		{dec("other.v1", "x", `{}`), ""},
	}

	for _, tt := range tests {
		if got := Link(tt.d); got != tt.want {
			t.Errorf("Link(%s) = %q, want %q", tt.d.Payload, got, tt.want)
		}
	}
}
