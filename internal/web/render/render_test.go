package render_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

type shellSource layout.Shell

func (s shellSource) Shell(*http.Request) layout.Shell { return layout.Shell(s) }

func newRenderer(theme layout.Theme) *render.Renderer {
	return render.New(shellSource{
		SiteName:    "TestSDR",
		Theme:       theme,
		FooterLinks: []layout.Link{{Label: "Usage policy", Href: "/policy"}},
	}, slog.New(slog.DiscardHandler))
}

func text(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	})
}

func TestWantsFragment(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"plain request", nil, false},
		{"htmx partial", map[string]string{"HX-Request": "true", "HX-Request-Type": "partial"}, true},
		{"htmx without type", map[string]string{"HX-Request": "true"}, true},
		{"boosted navigation", map[string]string{"HX-Request": "true", "HX-Boosted": "true", "HX-Request-Type": "full"}, false},
		{"boosted without type", map[string]string{"HX-Request": "true", "HX-Boosted": "true"}, false},
		{"history restore", map[string]string{"HX-Request": "true", "HX-History-Restore-Request": "true"}, false},
		{"htmx full (select)", map[string]string{"HX-Request": "true", "HX-Request-Type": "full"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}

			if got := render.WantsFragment(r); got != tt.want {
				t.Errorf("WantsFragment = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPage(t *testing.T) {
	rd := newRenderer(layout.ThemeAuto)
	page := layout.Page{Title: "Map", Section: "map"}

	tests := []struct {
		name     string
		headers  map[string]string
		fragment templ.Component
		full     bool
	}{
		{"full page", nil, text("<p>frag</p>"), true},
		{"fragment", map[string]string{"HX-Request": "true"}, text("<p>frag</p>"), false},
		{"boosted gets full page", map[string]string{"HX-Request": "true", "HX-Boosted": "true"}, text("<p>frag</p>"), true},
		{"no fragment representation", map[string]string{"HX-Request": "true"}, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/map", nil)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}

			rec := httptest.NewRecorder()
			rd.Page(rec, r, http.StatusOK, page, text("<p>content</p>"), tt.fragment)

			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
			}

			if got := rec.Header().Get("Vary"); got != render.Vary {
				t.Errorf("Vary = %q", got)
			}

			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}

			body := rec.Body.String()
			if tt.full {
				for _, want := range []string{"<!doctype html>", `<main id="main"`, `data-section="map"`, "<p>content</p>", "<title>Map · TestSDR</title>"} {
					if !strings.Contains(body, want) {
						t.Errorf("full page lacks %q:\n%s", want, body)
					}
				}
			} else if body != "<p>frag</p>" {
				t.Errorf("fragment = %q", body)
			}
		})
	}
}

// TestDocument checks the shell contract (ADR 0003 §6, UI-009): language,
// landmarks, skip link, live region, theme attributes, zoomable viewport,
// nonce'd scripts and the boost configuration.
func TestDocument(t *testing.T) {
	themes := []struct {
		theme       layout.Theme
		colorScheme string
		themeColors []string
	}{
		{layout.ThemeLight, "light", []string{`<meta name="theme-color" content="#ffffff">`}},
		{layout.ThemeDark, "dark", []string{`<meta name="theme-color" content="#161b22">`}},
		{layout.ThemeAuto, "light dark", []string{
			`<meta name="theme-color" content="#ffffff" media="(prefers-color-scheme: light)">`,
			`<meta name="theme-color" content="#161b22" media="(prefers-color-scheme: dark)">`,
		}},
	}

	for _, tt := range themes {
		t.Run(string(tt.theme), func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r = r.WithContext(templ.WithNonce(r.Context(), "n0nce"))

			rec := httptest.NewRecorder()
			newRenderer(tt.theme).Page(rec, r, http.StatusOK, layout.Page{}, text("<h1>Home</h1>"), nil)

			body := rec.Body.String()
			want := append([]string{
				`<html lang="en" data-theme="` + string(tt.theme) + `">`,
				`<meta name="color-scheme" content="` + tt.colorScheme + `">`,
				`<meta name="viewport" content="width=device-width, initial-scale=1">`,
				`<title>TestSDR</title>`,
				`<a href="#main" class="skip-link">`,
				`<header `,
				`<main id="main" tabindex="-1" hx-history-elt`,
				`<footer `,
				`<a href="/policy">Usage policy</a>`,
				`id="msdr-announcer" class="sr-only" aria-live="polite"`,
				`<script src="/static/vendor/htmx.min.js" nonce="n0nce" defer>`,
				`<script src="/static/js/shell.js" type="module" nonce="n0nce">`,
				`hx-boost:inherited='target:"#main" select:"#main" swap:"outerHTML"'`,
			}, tt.themeColors...)

			for _, w := range want {
				if !strings.Contains(body, w) {
					t.Errorf("document lacks %q:\n%s", w, body)
				}
			}

			for _, banned := range []string{"maximum-scale", "user-scalable"} {
				if strings.Contains(body, banned) {
					t.Errorf("viewport restricts zoom (%s)", banned)
				}
			}
		})
	}
}

func TestError(t *testing.T) {
	rd := newRenderer(layout.ThemeAuto)

	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusInternalServerError} {
		rec := httptest.NewRecorder()
		rd.Error(rec, httptest.NewRequest(http.MethodGet, "/nope", nil), status)

		body := rec.Body.String()
		if rec.Code != status || !strings.Contains(body, `<main id="main"`) ||
			!strings.Contains(body, "<h1 class=\"text-2xl font-semibold\">"+http.StatusText(status)+"</h1>") {
			t.Errorf("status %d: got %d\n%s", status, rec.Code, body)
		}
	}
}

func TestRenderFailure(t *testing.T) {
	broken := templ.ComponentFunc(func(context.Context, io.Writer) error { return errors.New("boom") })

	rec := httptest.NewRecorder()
	newRenderer(layout.ThemeAuto).Page(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, layout.Page{}, broken, nil)

	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "<main") {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
}
