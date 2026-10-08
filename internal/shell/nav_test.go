package shell_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/shell"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// navRouter serves the shell with an admin gate that is open or closed.
func navRouter(admin bool) http.Handler {
	gate := shell.GateFunc(func(context.Context) bool { return admin })
	m := shell.New(shell.Deps{Settings: values{}, AdminGate: gate, Logger: discard})
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	return httpserver.NewRouter(discard, "", api, m.HTTP)
}

var navLink = regexp.MustCompile(`<a href="([^"]+)" data-section="([a-z]+)" class="nav-link"( aria-current="page")?[^>]*>`)

// navOf returns the section links of the main nav: "href section[*]", *
// marking the current one.
func navOf(t *testing.T, body string) []string {
	t.Helper()

	start := strings.Index(body, `<nav aria-label="Main"`)
	if start < 0 {
		t.Fatalf("no main nav:\n%s", body)
	}

	end := strings.Index(body[start:], "</nav>")

	var out []string
	for _, m := range navLink.FindAllStringSubmatch(body[start:start+end], -1) {
		s := m[1] + " " + m[2]
		if m[3] != "" {
			s += "*"
		}

		out = append(out, s)
	}

	return out
}

// TestNavigation covers UI-006: the top bar lists the sections the visitor
// may open, in order, with the current one marked; Admin only through its
// gate (identity: admin role from an allowed network).
func TestNavigation(t *testing.T) {
	tests := []struct {
		name  string
		admin bool
		path  string
		want  []string
	}{
		{"visitor home", false, "/", []string{"/ receiver*", "/map map", "/decodes decodes", "/files files"}},
		{"visitor map", false, "/map", []string{"/ receiver", "/map map*", "/decodes decodes", "/files files"}},
		{"admin decodes", true, "/decodes", []string{"/ receiver", "/map map", "/decodes decodes*", "/files files", "/admin admin"}},
		{"admin policy", true, "/policy", []string{"/ receiver", "/map map", "/decodes decodes", "/files files", "/admin admin"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, body := do(t, navRouter(tt.admin), http.MethodGet, tt.path, nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d", tt.path, res.StatusCode)
			}

			if got := navOf(t, body); !slices.Equal(got, tt.want) {
				t.Errorf("nav = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSectionPages covers the section entry pages (the receiver island, and
// the placeholders of the sections whose modules come later): in the shell,
// in their section, with GET and HEAD.
func TestSectionPages(t *testing.T) {
	h := navRouter(false)

	tests := []struct{ path, title, section, text string }{
		{"/", "<title>MeshSDR</title>", "receiver", `<script id="msdr-receiver-config" type="application/json"`},
		{"/map", "<title>Map · MeshSDR</title>", "map", "The live map is not available yet."},
		{"/decodes", "<title>Decodes · MeshSDR</title>", "decodes", "Decoded messages are not available yet."},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			res, body := do(t, h, http.MethodGet, tt.path, nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET = %d", res.StatusCode)
			}

			for _, want := range []string{tt.title, `data-section="` + tt.section + `" class="mx-auto`, "<h1 ", tt.text} {
				if !strings.Contains(body, want) {
					t.Errorf("body lacks %q", want)
				}
			}

			if res, _ := do(t, h, http.MethodHead, tt.path, nil); res.StatusCode != http.StatusOK {
				t.Errorf("HEAD = %d", res.StatusCode)
			}

			// A boosted navigation gets the full page (htmx keeps #main).
			_, boosted := do(t, h, http.MethodGet, tt.path, map[string]string{"HX-Request": "true", "HX-Boosted": "true"})
			if !strings.Contains(boosted, `<main id="main"`) {
				t.Error("boosted navigation did not get the full page")
			}
		})
	}
}

// TestReceiverBookmarksLink covers BMK-001: the receiver's initial state
// names Bookmarks › Manage only for the visitors who may manage bookmarks.
func TestReceiverBookmarksLink(t *testing.T) {
	tests := []struct {
		name  string
		link  *shell.BookmarksLink
		allow bool
		want  bool
	}{
		{"no link", nil, false, false},
		{"listener", &shell.BookmarksLink{Path: "/bookmarks/manage"}, false, false},
		{"operator", &shell.BookmarksLink{Path: "/bookmarks/manage"}, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.link != nil {
				tt.link.Gate = shell.GateFunc(func(context.Context) bool { return tt.allow })
			}

			m := shell.New(shell.Deps{Settings: values{}, Bookmarks: tt.link, Logger: discard})
			h := httpserver.NewRouter(discard, "", http.NotFoundHandler(), m.HTTP)

			_, body := do(t, h, http.MethodGet, "/", nil)
			if got := strings.Contains(body, `"bookmarks_url":"/bookmarks/manage"`); got != tt.want {
				t.Errorf("bookmarks_url present = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestUTCClock covers the top bar clock: HH:MM UTC of the render time, with
// a machine-readable datetime, not a live region.
func TestUTCClock(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 10, 6, 21, 7, 59, 0, time.FixedZone("CEST", 2*3600)) }
	m := shell.New(shell.Deps{Settings: values{}, Now: now, Logger: discard})
	h := httpserver.NewRouter(discard, "", http.NotFoundHandler(), m.HTTP)

	_, body := do(t, h, http.MethodGet, "/policy", nil)

	want := `<msdr-utc-clock class="whitespace-nowrap font-mono tabular-nums text-fg-muted"><time datetime="2026-10-06T19:07Z">19:07</time> UTC</msdr-utc-clock>`
	if !strings.Contains(body, want) {
		t.Errorf("body lacks %s", want)
	}
}

// TestUserMenu covers UI-010: anonymous visitors get a Sign in link; signed
// in users a menu with their name, role, account links, the usage policy
// and Sign out (a POST to /logout). The notifications area is always there.
func TestUserMenu(t *testing.T) {
	user := func(r *http.Request) *layout.User {
		if r.Header.Get("X-Test-User") == "" {
			return nil
		}

		return &layout.User{Name: "Ada <Lovelace>", Role: "Operator", Links: []layout.Link{{Label: "Change password", Href: "/account/password"}}}
	}

	m := shell.New(shell.Deps{Settings: values{}, User: user, Logger: discard})
	h := httpserver.NewRouter(discard, "", http.NotFoundHandler(), m.HTTP)

	_, anon := do(t, h, http.MethodGet, "/", nil)
	for _, want := range []string{`<a href="/login" class="font-medium">Sign in</a>`, `popovertarget="msdr-notifications"`, `id="msdr-notifications" popover`} {
		if !strings.Contains(anon, want) {
			t.Errorf("anonymous page lacks %s", want)
		}
	}

	if strings.Contains(anon, "msdr-user-menu") || strings.Contains(anon, "/logout") {
		t.Error("anonymous page has a user menu")
	}

	_, signed := do(t, h, http.MethodGet, "/", map[string]string{"X-Test-User": "1"})
	for _, want := range []string{
		`popovertarget="msdr-user-menu"`,
		`<span class="font-semibold">Ada &lt;Lovelace&gt;</span>`,
		`<span class="badge">Operator</span>`,
		`<a href="/account/password" class="menu-item">Change password</a>`,
		`<a href="/policy" class="menu-item">Usage policy</a>`,
		`<a href="/about" class="menu-item">About</a>`,
		`<form method="post" action="/logout"><button type="submit" class="menu-item">Sign out</button>`,
	} {
		if !strings.Contains(signed, want) {
			t.Errorf("signed-in page lacks %s", want)
		}
	}

	if strings.Contains(signed, `href="/login"`) {
		t.Error("signed-in page links to the login page")
	}
}

// TestAbout covers the About page (UI-010) and the footer: version, licence
// and a link to the source code (AGPL-3.0 section 13), on every page.
func TestAbout(t *testing.T) {
	h := router(values{})

	res, body := do(t, h, http.MethodGet, "/about", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /about = %d", res.StatusCode)
	}

	for _, want := range []string{
		"<title>About · MeshSDR</title>", "<h1>About</h1>", "<dd>AGPL-3.0-or-later</dd>",
		`<a href="https://github.com/yohang/mesh-sdr" rel="noopener noreferrer" hx-boost="false">https://github.com/yohang/mesh-sdr</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("about page lacks %s", want)
		}
	}

	_, home := do(t, h, http.MethodGet, "/map", nil)
	for _, want := range []string{
		`<li><a href="/about">About</a></li>`, "· AGPL-3.0-or-later</li>",
		`<li><a href="https://github.com/yohang/mesh-sdr" rel="noopener noreferrer" hx-boost="false">Source code</a></li>`,
	} {
		if !strings.Contains(home, want) {
			t.Errorf("footer lacks %s", want)
		}
	}

	if res, _ := do(t, h, http.MethodHead, "/about", nil); res.StatusCode != http.StatusOK {
		t.Errorf("HEAD /about = %d", res.StatusCode)
	}
}

// TestHelp covers UI-002: with receiver.help_url set, the help link opens in
// a new tab from the top bar (key H through data-shortcut), the user menu
// and the footer; unset, every entry is hidden. ui.shortcut_set = off turns
// single-key shortcuts off.
func TestHelp(t *testing.T) {
	user := func(*http.Request) *layout.User { return &layout.User{Name: "ada", Role: "Listener"} }

	page := func(v values) string {
		t.Helper()

		m := shell.New(shell.Deps{Settings: v, User: user, Logger: discard})
		_, body := do(t, httpserver.NewRouter(discard, "", http.NotFoundHandler(), m.HTTP), http.MethodGet, "/map", nil)

		return body
	}

	body := page(values{"receiver.help_url": "https://docs.example.org/rx"})
	for _, want := range []string{
		`<a href="https://docs.example.org/rx" target="_blank" rel="noopener noreferrer" hx-boost="false" class="icon-button" data-shortcut="h" aria-label="Help (opens in a new tab)" aria-keyshortcuts="h">`,
		`<a href="https://docs.example.org/rx" class="menu-item" target="_blank" rel="noopener noreferrer" hx-boost="false">Help<span class="sr-only"> (opens in a new tab)</span></a>`,
		`<li><a href="https://docs.example.org/rx" target="_blank" rel="noopener noreferrer" hx-boost="false">Help<span class="sr-only"> (opens in a new tab)</span></a></li>`,
		`data-shortcuts="default"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}

	body = page(values{"ui.shortcut_set": "off"})
	if strings.Contains(body, "docs.example.org") || strings.Contains(body, `data-shortcut="h"`) || strings.Contains(body, ">Help") {
		t.Error("help entries shown without a help link")
	}

	if !strings.Contains(body, `data-shortcuts="off"`) {
		t.Error("shortcuts not turned off")
	}

	// An invalid stored value hides the link rather than rendering it.
	if body := page(values{"receiver.help_url": "javascript:alert(1)"}); strings.Contains(body, "javascript:") {
		t.Error("invalid help link rendered")
	}
}

type images map[string]bool

func (i images) HasImage(_ context.Context, slot string) bool { return i[slot] }

// TestReceiverStation covers the Receiver page until the receiver exists:
// the station's name, location, images and description (raw HTML dropped).
func TestReceiverStation(t *testing.T) {
	v := values{
		"receiver.name": "F4XYZ SDR", "receiver.location": "Lille, France", "receiver.photo_title": "The <antenna>",
		"receiver.photo_desc": "A **loop** on the roof.<script>alert(1)</script>",
	}

	m := shell.New(shell.Deps{Settings: v, Images: images{"avatar": true, "panorama": true}, Logger: discard})
	_, body := do(t, httpserver.NewRouter(discard, "", http.NotFoundHandler(), m.HTTP), http.MethodGet, "/", nil)

	for _, want := range []string{
		`<img src="/api/v1/branding/avatar" alt=""`, `<h1 class="text-2xl font-semibold">F4XYZ SDR</h1>`,
		`<p class="text-fg-muted">Lille, France</p>`, `<img src="/api/v1/branding/panorama" alt="The &lt;antenna&gt;"`,
		`<p class="font-semibold">The &lt;antenna&gt;</p>`, `<strong>loop</strong>`,
		// The station block the island moves into its Info panel (RX-036).
		`<div data-rx-station`, `<dd class="min-w-0 break-words">Lille, France</dd>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("receiver page lacks %s", want)
		}
	}

	if strings.Contains(body, "<script>alert") {
		t.Error("raw HTML of the description rendered")
	}

	// Without images nor description, only the name and the notice.
	m = shell.New(shell.Deps{Settings: values{}, Logger: discard})
	_, body = do(t, httpserver.NewRouter(discard, "", http.NotFoundHandler(), m.HTTP), http.MethodGet, "/", nil)

	if strings.Contains(body, "/api/v1/branding/") || strings.Contains(body, "<figure") {
		t.Error("empty station shows images or a description")
	}

	// The island asks the nodes for the codec of audio_compression (DEM-010).
	if !strings.Contains(body, `"audio_codec":"adpcm-ima"`) {
		t.Error("default audio codec is not adpcm-ima")
	}

	m = shell.New(shell.Deps{Settings: values{"audio_compression": "pcm"}, Logger: discard})
	_, body = do(t, httpserver.NewRouter(discard, "", http.NotFoundHandler(), m.HTTP), http.MethodGet, "/", nil)

	if !strings.Contains(body, `"audio_codec":"pcm-s16le"`) {
		t.Error("audio_compression = pcm does not ask for pcm-s16le")
	}
}

// TestReceiverRecorder: the island offers the browser recorder (REC-001)
// with ui.recorder_enabled, and to admins whatever the setting.
func TestReceiverRecorder(t *testing.T) {
	admin := shell.GateFunc(func(context.Context) bool { return true })
	nobody := shell.GateFunc(func(context.Context) bool { return false })

	for _, tc := range []struct {
		name    string
		enabled string
		gate    shell.Gate
		want    bool
	}{
		{"enabled", "true", nobody, true},
		{"disabled", "false", nobody, false},
		{"disabled, no admin gate", "false", nil, false},
		{"disabled, admin", "false", admin, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := shell.New(shell.Deps{Settings: values{"ui.recorder_enabled": tc.enabled}, AdminGate: tc.gate, Logger: discard})
			_, body := do(t, httpserver.NewRouter(discard, "", http.NotFoundHandler(), m.HTTP), http.MethodGet, "/", nil)

			if want := fmt.Sprintf(`"recorder":%t`, tc.want); !strings.Contains(body, want) {
				t.Errorf("receiver config lacks %s", want)
			}
		})
	}
}
