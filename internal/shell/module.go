package shell

import (
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/version"
	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

//go:generate go tool templ generate

// Module is the shell's router module (internal/http.Module) and the
// shell data of every page (render.ShellSource).
type Module struct {
	render   *render.Renderer
	settings Settings
	gates    map[string]Gate
	user     func(r *http.Request) *layout.User
	images   StationImages
	static   fs.FS
	markdown *markdown
	// admin tells whether the visitor is an admin, who always gets the
	// browser recorder (REC-001); nil: nobody.
	admin Gate
	// bookmarks is the Bookmarks › Manage link of the receiver (nil: none).
	bookmarks *BookmarksLink
	now       func() time.Time
	logger    *slog.Logger
}

// Shell implements render.ShellSource.
func (m *Module) Shell(r *http.Request) layout.Shell {
	var user *layout.User
	if m.user != nil {
		user = m.user(r)
	}

	return layout.Shell{
		User:        user,
		SiteName:    m.settings.SiteName(),
		Theme:       m.settings.Theme(),
		FooterLinks: m.footerLinks(),
		HelpURL:     m.settings.HelpURL(),
		Shortcuts:   m.settings.Shortcuts(),
		Product:     product(),
		Nav:         nav(r.Context(), m.gates),
		Now:         m.now(),
	}
}

// Middlewares implements internal/http.Module: the shell has none.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module. The shell owns the HTML 404 and
// 405 pages of the whole router; the API keeps its problem+json errors.
func (m *Module) Routes(r chi.Router) {
	r.NotFound(m.render.NotFound)
	r.MethodNotAllowed(m.methodNotAllowed)

	// Read-only pages also answer HEAD: the router serves it with the GET
	// route (chi middleware.GetHead; net/http drops HEAD bodies).
	r.Get("/", m.receiver)
	r.Get(ReceiverLinkPattern, m.receiverLink)
	r.Get("/robots.txt", robots)
	r.Get("/policy", m.policyPage)
	r.Get(AboutPath, m.aboutPage)
	r.Get("/manifest.webmanifest", m.manifest)
	r.Get("/favicon.ico", favicon(m.static))
}

// allowCandidates are the methods probed for the Allow header of a 405.
var allowCandidates = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// methodNotAllowed is the router's 405 page. chi gives a custom 405 handler
// no Allow header, so the methods the path accepts are probed on the router.
func (m *Module) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.Routes != nil {
		var allowed []string

		for _, method := range allowCandidates {
			// HEAD is served by the GET route (middleware.GetHead).
			probe := method
			if method == http.MethodHead {
				probe = http.MethodGet
			}

			if rctx.Routes.Match(chi.NewRouteContext(), probe, r.URL.Path) {
				allowed = append(allowed, method)
			}
		}

		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
		}
	}

	m.render.MethodNotAllowed(w, r)
}

// receiverConfig is the initial state of the <msdr-receiver> island
// (templ.JSONScript): the island lists the devices itself (GET
// /api/v1/features) and offers to sign in when it may list none.
// AudioCodec is the audio codec it asks the nodes for (audio_compression).
// BookmarksURL is the Bookmarks › Manage page, set for the visitors who may
// add hub bookmarks (operators and admins): the Bookmarks tab links its add
// form, pre-filled from the tuning.
// Recorder shows the Record button and the R shortcut (REC-001): to every
// listener with ui.recorder_enabled, to admins always. It is a convenience,
// not an enforcement: the audio reaches the browser anyway.
type receiverConfig struct {
	SignedIn     bool   `json:"signed_in"`
	LoginURL     string `json:"login_url"`
	AudioCodec   string `json:"audio_codec"`
	BookmarksURL string `json:"bookmarks_url,omitempty"`
	Recorder     bool   `json:"recorder"`
}

// receiver is the Receiver section's entry page: the station (name,
// location, images, description) and the receiver island.
func (m *Module) receiver(w http.ResponseWriter, r *http.Request) {
	st := m.station(r.Context())

	desc, err := m.markdown.HTML(st.PhotoDesc)
	if err != nil {
		m.logger.WarnContext(r.Context(), "render station description", slog.Any("error", err))

		desc = ""
	}

	rx := receiverConfig{
		SignedIn: m.user != nil && m.user(r) != nil, LoginURL: "/login", AudioCodec: st.AudioCodec,
		Recorder: st.RecorderEnabled || (m.admin != nil && m.admin.Allows(r.Context())),
	}
	if b := m.bookmarks; b != nil && b.Gate != nil && b.Gate.Allows(r.Context()) {
		rx.BookmarksURL = b.Path
	}

	page := layout.Page{Section: layout.SectionReceiver}
	m.render.Page(w, r, http.StatusOK, page, receiverPage(m.settings.SiteName(), st, desc, rx), nil)
}

// ReceiverLinkPattern is the deep link of a device (RX-028):
// /receiver/{nodeId}/{deviceId}?f=<Hz>&m=<mod>&m2=<mod>&sql=<dB>. The
// query is read by the island only.
const ReceiverLinkPattern = "/receiver/{nodeId}/{deviceId}"

// linkID is the §6.1 identifier charset, which node and device ids keep to.
var linkID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// receiverLink serves a deep link: the same receiver page, whose island
// selects the linked device for this visitor only and tunes its own
// demodulator (it never switches the preset nor moves the centre). An
// unknown or unavailable device is the island's "device unavailable"
// state, so a link to a device the visitor may not list does not reveal
// whether it exists; a malformed id is a 404.
func (m *Module) receiverLink(w http.ResponseWriter, r *http.Request) {
	if !linkID.MatchString(chi.URLParam(r, "nodeId")) || !linkID.MatchString(chi.URLParam(r, "deviceId")) {
		m.render.NotFound(w, r)

		return
	}

	m.receiver(w, r)
}

// policyPage serves the usage policy (UI-003), public.
func (m *Module) policyPage(w http.ResponseWriter, r *http.Request) {
	html, err := m.markdown.HTML(m.settings.Policy())
	if err != nil {
		m.logger.ErrorContext(r.Context(), "render usage policy", slog.Any("error", err))
		m.render.Error(w, r, http.StatusInternalServerError)

		return
	}

	m.render.Page(w, r, http.StatusOK, layout.Page{Title: "Usage policy"}, policyPage(html), nil)
}

// AboutPath is the About page (UI-010).
const AboutPath = "/about"

// product describes the running build.
func product() layout.Product {
	return layout.Product{Name: "MeshSDR", Version: version.String(), License: version.License, SourceURL: version.SourceURL()}
}

// aboutPage serves the About page: product, version, licence and the link
// to the source code of this build (AGPL-3.0 section 13), public.
func (m *Module) aboutPage(w http.ResponseWriter, r *http.Request) {
	m.render.Page(w, r, http.StatusOK, layout.Page{Title: "About"}, aboutPage(product(), m.settings.HelpURL()), nil)
}

// footerLinks are the information links of the footer and the user menu:
// help (when set), the usage policy and About.
func (m *Module) footerLinks() []layout.Link {
	var links []layout.Link
	if help := m.settings.HelpURL(); help != "" {
		links = append(links, layout.Link{Label: "Help", Href: help, External: true})
	}

	return append(links, layout.Link{Label: "Usage policy", Href: m.settings.PolicyURL()}, layout.Link{Label: "About", Href: AboutPath})
}
