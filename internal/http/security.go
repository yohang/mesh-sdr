package http

import (
	"crypto/rand"
	"fmt"
	"net/http"

	"github.com/a-h/templ"
)

// cspTemplate is the Content-Security-Policy (TECHNICAL_SPEC §2.3 security baseline).
//
//   - script-src: nonce only, no 'self': a script runs only if the page that rendered it
//     said so, so a same-origin file (e.g. a user upload served by Files) cannot be
//     loaded as a script. No 'unsafe-eval': htmx features that evaluate JS (hx-on,
//     js: expressions) are not used.
//   - style-src 'self': no inline <style> or style="" attributes. htmx 4 injects its
//     indicator CSS as a constructable stylesheet, which this does not block.
//   - connect-src 'self': fetch/htmx and same-origin WebSockets (/api/ws, node media WS).
const cspTemplate = "default-src 'none'; " +
	"script-src 'nonce-%s'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"media-src 'self' blob:; " +
	"manifest-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'"

// securityHeaders sets the baseline security headers and a fresh CSP nonce per request.
// The nonce is stored in the request context with templ.WithNonce, so templ components
// read it with templ.GetNonce(ctx) and templ.JSONScript picks it up automatically.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 128-bit CSPRNG value, base32: valid CSP nonce characters.
		nonce := rand.Text()

		h := w.Header()
		h.Set("Content-Security-Policy", fmt.Sprintf(cspTemplate, nonce))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")

		next.ServeHTTP(w, r.WithContext(templ.WithNonce(r.Context(), nonce)))
	})
}
