package shell_test

import (
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"

	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/shell"
)

var discard = slog.New(slog.DiscardHandler)

// values are effective settings by key, over the defaults the shell reads.
type values map[string]string

func (v values) String(key string) string {
	if s, ok := v[key]; ok {
		return s
	}

	return map[string]string{"ui.theme_mode": "auto", "receiver.name": "MeshSDR", "receiver.usage_policy_url": "/policy", "ui.shortcut_set": "default"}[key]
}

// Bool reads a boolean setting stored as "true" or "false"; the defaults
// the shell reads apply otherwise.
func (v values) Bool(key string) bool {
	if s, ok := v[key]; ok {
		return s == "true"
	}

	return key == "ui.recorder_enabled"
}

// router serves the shell module the way the hub does, with a stub API.
func router(settings values) http.Handler {
	m := shell.New(shell.Deps{Settings: settings, Logger: discard})
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	return httpserver.NewRouter(discard, "", api, m.HTTP)
}

func do(t *testing.T, h http.Handler, method, path string, headers map[string]string) (*http.Response, string) {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	res := rec.Result()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}

	return res, string(body)
}

// TestThemeMode covers UI-001/UI-008: the configured theme mode drives the
// root element and color-scheme; an invalid stored value falls back to auto.
func TestThemeMode(t *testing.T) {
	tests := []struct {
		mode, want, colorScheme string
	}{
		{"light", "light", "light"},
		{"dark", "dark", "dark"},
		{"auto", "auto", "light dark"},
		{"sepia", "auto", "light dark"}, // never reaches here through config validation
	}

	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			h := router(values{"ui.theme_mode": tt.mode})

			res, body := do(t, h, http.MethodGet, "/", nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET / = %d", res.StatusCode)
			}

			for _, want := range []string{
				`<html lang="en" data-theme="` + tt.want + `">`,
				`<meta name="color-scheme" content="` + tt.colorScheme + `">`,
				`<h1 class="text-2xl font-semibold">MeshSDR</h1>`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("body lacks %q", want)
				}
			}
		})
	}
}

// TestRobots covers UI-005.
func TestRobots(t *testing.T) {
	res, body := do(t, router(nil), http.MethodGet, "/robots.txt", nil)

	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("got %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}

	lines := map[string]bool{}
	for line := range strings.SplitSeq(body, "\n") {
		lines[line] = true
	}

	for _, want := range []string{"User-agent: *", "Disallow: /login", "Disallow: /logout", "Disallow: /admin", "Disallow: /api/", "Disallow: /nodes/"} {
		if !lines[want] {
			t.Errorf("robots.txt lacks %q:\n%s", want, body)
		}
	}
}

// TestPolicy covers UI-003: public page with the default or the configured
// policy, rendered without raw HTML, linked from the footer.
func TestPolicy(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		want   []string
		banned []string
	}{
		{"default", "", []string{"<h1>Usage policy</h1>", "<h2>Acceptable use</h2>"}, nil},
		{"configured", "# House rules\n\nBe <em>nice</em> & [polite](javascript:alert(1)).\n<script>alert(1)</script>", []string{
			"<h1>Usage policy</h1>", "<h2>House rules</h2>", "Be ", "nice", " &amp; ",
		}, []string{"<em>", "<script>alert", "javascript:", "Acceptable use"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := router(values{"receiver.usage_policy_text": tt.text})

			res, body := do(t, h, http.MethodGet, "/policy", nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET /policy = %d", res.StatusCode)
			}

			for _, w := range append(tt.want, "<title>Usage policy · MeshSDR</title>", `<a href="/policy">Usage policy</a>`) {
				if !strings.Contains(body, w) {
					t.Errorf("body lacks %q", w)
				}
			}

			for _, b := range tt.banned {
				if strings.Contains(body, b) {
					t.Errorf("body contains %q", b)
				}
			}
		})
	}
}

// TestManifestAndIcons covers UI-004: the manifest carries the name, the
// theme colors of the mode and its icons, and every icon linked from the
// document or the manifest is served with its type and declared size.
func TestManifestAndIcons(t *testing.T) {
	colors := map[string]string{"light": "#ffffff", "dark": "#161b22", "auto": "#ffffff"}

	for mode, color := range colors {
		t.Run(mode, func(t *testing.T) {
			h := router(values{"ui.theme_mode": mode})

			res, body := do(t, h, http.MethodGet, "/manifest.webmanifest", nil)
			if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/manifest+json" {
				t.Fatalf("manifest: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
			}

			var m struct {
				Name            string
				ShortName       string `json:"short_name"`
				StartURL        string `json:"start_url"`
				Display         string
				BackgroundColor string `json:"background_color"`
				ThemeColor      string `json:"theme_color"`
				Icons           []struct{ Src, Sizes, Type, Purpose string }
			}
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Fatal(err)
			}

			if m.Name != "MeshSDR" || m.ShortName != "MeshSDR" || m.StartURL != "/" || m.Display != "standalone" || m.ThemeColor != color || m.BackgroundColor != color {
				t.Errorf("manifest = %+v", m)
			}

			var maskable, large bool

			for _, icon := range m.Icons {
				checkIcon(t, h, icon.Src, icon.Type, icon.Sizes)

				maskable = maskable || icon.Purpose == "maskable"
				large = large || icon.Sizes == "512x512"
			}

			if !maskable || !large {
				t.Errorf("icons = %+v: want a 512px and a maskable icon", m.Icons)
			}
		})
	}

	// Every icon and manifest link of the document resolves.
	_, page := do(t, router(nil), http.MethodGet, "/", nil)

	links := regexp.MustCompile(`<link rel="(icon|apple-touch-icon|manifest)" href="([^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(links) < 4 {
		t.Fatalf("head links = %v", links)
	}

	h := router(nil)
	types := map[string]string{".ico": "image/vnd.microsoft.icon", ".svg": "image/svg+xml", ".png": "image/png", ".webmanifest": "application/manifest+json"}

	for _, l := range links {
		checkIcon(t, h, l[2], types[path.Ext(l[2])], "")
	}
}

// checkIcon fetches src and checks its type and, for PNGs, its size.
func checkIcon(t *testing.T, h http.Handler, src, ctype, sizes string) {
	t.Helper()

	res, body := do(t, h, http.MethodGet, src, nil)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != ctype {
		t.Errorf("%s: %d %q, want 200 %q", src, res.StatusCode, res.Header.Get("Content-Type"), ctype)

		return
	}

	switch ctype {
	case "image/png":
		img, err := png.Decode(strings.NewReader(body))
		if err != nil {
			t.Errorf("%s: %v", src, err)

			return
		}

		if b := img.Bounds(); sizes != "" && fmt.Sprintf("%dx%d", b.Dx(), b.Dy()) != sizes {
			t.Errorf("%s: %v, want %s", src, b.Size(), sizes)
		}
	case "image/vnd.microsoft.icon":
		if !strings.HasPrefix(body, "\x00\x00\x01\x00") {
			t.Errorf("%s: not an ICO file", src)
		}
	case "image/svg+xml":
		if !strings.HasPrefix(body, "<svg") {
			t.Errorf("%s: not an SVG", src)
		}
	}
}

func TestMethods(t *testing.T) {
	h := router(nil)

	for _, p := range []string{"/", "/policy", "/robots.txt", "/manifest.webmanifest", "/favicon.ico"} {
		// The recorder keeps the body; net/http drops it on the wire.
		res, _ := do(t, h, http.MethodHead, p, nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("HEAD %s = %d", p, res.StatusCode)
		}
	}

	res, _ := do(t, h, http.MethodPost, "/policy", nil)
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /policy = %d, Allow %q", res.StatusCode, res.Header.Get("Allow"))
	}
}

// TestReceiverDeepLink covers RX-028: a deep link renders the receiver page
// (the island reads the device and the tuning from the URL), for unknown
// devices too; malformed ids are a 404.
func TestReceiverDeepLink(t *testing.T) {
	h := router(nil)

	for _, tt := range []struct {
		path   string
		status int
	}{
		{"/receiver/attic/hf-1?f=7074000&m=usb&sql=-90", http.StatusOK},
		{"/receiver/local/no-such-device", http.StatusOK},
		{"/receiver/attic/bad%20id", http.StatusNotFound},
		{"/receiver/attic", http.StatusNotFound},
	} {
		t.Run(tt.path, func(t *testing.T) {
			res, body := do(t, h, http.MethodGet, tt.path, nil)
			if res.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.status)
			}

			island := strings.Contains(body, "<msdr-receiver")
			if island != (tt.status == http.StatusOK) {
				t.Errorf("receiver island = %v", island)
			}

			if island && !strings.Contains(body, `data-section="receiver"`) {
				t.Error("deep link is not in the Receiver section")
			}
		})
	}
}

func TestErrorPages(t *testing.T) {
	h := router(nil)

	tests := []struct {
		method, path string
		headers      map[string]string
		status       int
		html         bool
	}{
		{http.MethodGet, "/nope", nil, http.StatusNotFound, true},
		{http.MethodGet, "/nope", map[string]string{"HX-Request": "true", "HX-Boosted": "true"}, http.StatusNotFound, true},
		{http.MethodDelete, "/", nil, http.StatusMethodNotAllowed, true},
		{http.MethodGet, httpserver.APIPrefix + "/nope", nil, http.StatusTeapot, false},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			res, body := do(t, h, tt.method, tt.path, tt.headers)
			if res.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.status)
			}

			isShell := strings.Contains(body, `<main id="main"`) && strings.Contains(body, "<h1")
			if isShell != tt.html {
				t.Errorf("shell page = %v, want %v:\n%s", isShell, tt.html, body)
			}
		})
	}
}
