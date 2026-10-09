package mapfeatures

import (
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/web/layout"
)

//go:generate go tool templ generate

// Path is the Map section (MAP-001).
const Path = "/map"

// pageConfig is the initial state of the <msdr-map> island
// (templ.JSONScript): the island loads the map configuration and the
// features itself (GET /api/v1/map/config, /map/features) and offers to
// sign in when it may list no device.
type pageConfig struct {
	SignedIn bool   `json:"signed_in"`
	LoginURL string `json:"login_url"`
}

// mapView is the data of the Map page.
type mapView struct {
	Config pageConfig
	// Now is the render time of the toolbar's UTC clock (MAP-018).
	Now time.Time
}

// Middlewares implements internal/http.Module.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module: the Map page, open to every
// visitor (what they see follows the listen policy of each device).
func (m *Module) Routes(r chi.Router) {
	r.Get(Path, m.page)
}

// ImageSources implements internal/http.ImageSources: the origins of the
// tiles of the offered base layers, for the CSP img-src (MAP-004).
func (m *Module) ImageSources() []string {
	if m.d.Config == nil {
		return nil
	}

	return TileOrigins(m.d.Config().BaseLayers)
}

// page serves the Map page (FEATURE_SPEC §10.10): the toolbar, the map
// island with its layers, legend and detail panels, and the list view.
func (m *Module) page(w http.ResponseWriter, r *http.Request) {
	v := mapView{Config: pageConfig{LoginURL: "/login"}, Now: m.d.Now()}
	if m.d.SignedIn != nil {
		v.Config.SignedIn = m.d.SignedIn(r.Context())
	}

	m.d.Render.Page(w, r, http.StatusOK, layout.Page{Title: "Map", Section: layout.SectionMap}, mapPage(v), nil)
}

// Link is the Map link of a decoded message (MAP-016): /map?callsign=<the
// station it locates>, "" when it puts nothing on the map.
func Link(d Decode) string {
	for _, r := range project(d, Settings{}).reports {
		if f := r.feature; f.Kind == KindAPRS || f.Kind == KindLocator {
			return Path + "?" + url.Values{"callsign": {f.Subject}}.Encode()
		}
	}

	return ""
}
