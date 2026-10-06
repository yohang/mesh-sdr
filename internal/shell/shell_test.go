package shell_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/config"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/shell"
)

var discard = slog.New(slog.DiscardHandler)

// router serves the shell module the way the hub does, with a stub API.
func router(settings config.Settings) http.Handler {
	m := shell.Wire(shell.Deps{Settings: settings, Logger: discard})
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	return httpserver.NewRouter(discard, api, m.HTTP)
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
			h := router(config.Settings{UI: config.SettingsUI{ThemeMode: tt.mode}})

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

func TestErrorPages(t *testing.T) {
	h := router(config.DefaultHub().Settings)

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
