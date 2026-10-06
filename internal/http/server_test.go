package http

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()

	// The shell routes do not touch the database.
	return NewRouter(slog.New(slog.DiscardHandler), nil)
}

func get(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	res := rec.Result()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}

	return res, string(body)
}

var (
	cspNonceRe  = regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]+)'`)
	htmlNonceRe = regexp.MustCompile(`nonce="([^"]*)"`)
	csrfMetaRe  = regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)">`)
)

func TestShellPages(t *testing.T) {
	h := newTestRouter(t)

	tests := []struct {
		path, current, title string
	}{
		{"/", "Receiver", "Receiver · MeshSDR"},
		{"/map", "Map", "Map · MeshSDR"},
		{"/decodes", "Decodes", "Decodes · MeshSDR"},
		{"/files", "Files", "Files · MeshSDR"},
		{"/admin", "Admin", "Admin · MeshSDR"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			res, body := get(t, h, tt.path)

			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.StatusCode)
			}
			if !strings.Contains(body, "<title>"+tt.title+"</title>") {
				t.Errorf("missing title %q", tt.title)
			}
			if !strings.Contains(body, `aria-current="page">`+tt.current+`</a>`) {
				t.Errorf("nav does not mark %q as current", tt.current)
			}
			if n := strings.Count(body, `aria-current="page"`); n != 1 {
				t.Errorf("aria-current count = %d, want 1", n)
			}
			for _, want := range []string{
				`data-theme="auto"`,
				`<meta name="color-scheme" content="light dark">`,
				`<a href="#main" class="skip-link">`,
				`<main id="main" tabindex="-1" hx-history-elt`,
				`<nav id="primary-nav" aria-label="Main"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q", want)
				}
			}
		})
	}
}

func TestNotFoundRendersInShell(t *testing.T) {
	res, body := get(t, newTestRouter(t), "/nope")

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if !strings.Contains(body, "Page not found") || !strings.Contains(body, `id="primary-nav"`) {
		t.Error("404 page is not rendered in the app shell")
	}
}

func TestCSPNonce(t *testing.T) {
	h := newTestRouter(t)

	res1, body1 := get(t, h, "/")
	res2, _ := get(t, h, "/")

	m1 := cspNonceRe.FindStringSubmatch(res1.Header.Get("Content-Security-Policy"))
	m2 := cspNonceRe.FindStringSubmatch(res2.Header.Get("Content-Security-Policy"))
	if m1 == nil || m2 == nil {
		t.Fatalf("CSP without script nonce: %q", res1.Header.Get("Content-Security-Policy"))
	}
	if m1[1] == m2[1] {
		t.Error("nonce reused across requests")
	}

	nonces := htmlNonceRe.FindAllStringSubmatch(body1, -1)
	if len(nonces) < 3 { // htmx, shell.js, receiver JSON script
		t.Fatalf("found %d nonce attributes, want at least 3", len(nonces))
	}
	for _, n := range nonces {
		if n[1] != m1[1] {
			t.Errorf("nonce attribute %q does not match the CSP nonce %q", n[1], m1[1])
		}
	}

	csp := res1.Header.Get("Content-Security-Policy")
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP contains %s", forbidden)
		}
	}
	for _, want := range []string{"frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %s", want)
		}
	}
	if got := res1.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := res1.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q", got)
	}
}

func TestReceiverJSONScriptEscapesUntrustedText(t *testing.T) {
	_, body := get(t, newTestRouter(t), "/")

	if strings.Contains(body, "<img src=x") {
		t.Error("untrusted device name rendered as markup")
	}
	if !strings.Contains(body, `<script id="receiver-bootstrap" type="application/json"`) {
		t.Error("missing receiver bootstrap JSON script")
	}
}

func TestCSRF(t *testing.T) {
	h := newTestRouter(t)

	res, body := get(t, h, "/admin")
	cookies := res.Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("expected the session cookie, got %v", cookies)
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Error("session cookie must be HttpOnly and SameSite=Lax")
	}

	m := csrfMetaRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("missing csrf-token meta")
	}
	token := m[1]

	tests := []struct {
		name        string
		cookie      bool
		token       string
		contentType string
		fetchSite   string
		want        int
	}{
		{"valid", true, token, "application/json", "same-origin", http.StatusOK},
		{"no session", false, token, "application/json", "same-origin", http.StatusForbidden},
		{"no token", true, "", "application/json", "same-origin", http.StatusForbidden},
		{"wrong token", true, token + "x", "application/json", "same-origin", http.StatusForbidden},
		{"cross site", true, token, "application/json", "cross-site", http.StatusForbidden},
		{"form body", true, token, "application/x-www-form-urlencoded", "same-origin", http.StatusUnsupportedMediaType},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/admin/demo", strings.NewReader(`{"note":"hi"}`))
			req.Header.Set("Content-Type", tt.contentType)
			req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			if tt.token != "" {
				req.Header.Set(CSRFHeader, tt.token)
			}
			if tt.cookie {
				req.AddCookie(cookies[0])
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}
