package http

import (
	"crypto/rand"
	"net/http"

	"github.com/a-h/templ"
)

// ContentSecurityPolicy returns the Content-Security-Policy sent with every
// response (TECHNICAL_SPEC §2.3, ADR 0003 §3):
//
//   - script-src: the per-request nonce only, no 'self' and no 'unsafe-eval'.
//     A same-origin file (for example a Files upload) cannot run as a script,
//     and htmx features that evaluate JS (hx-on, js: expressions) are blocked.
//     Modules imported by a nonce'd entry module inherit its nonce.
//   - style-src 'self': no inline <style> nor style="" attributes. htmx 4
//     injects its indicator CSS as a constructable stylesheet, which this
//     does not block.
//   - connect-src 'self': fetch, htmx and same-origin WebSockets.
func ContentSecurityPolicy(nonce string) string {
	return "default-src 'none'; " +
		"script-src 'nonce-" + nonce + "'; " +
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
}

// PermissionsPolicy denies every powerful browser feature by default
// (TECHNICAL_SPEC SR-28). The receiver only plays audio, which needs none of
// them; a feature that needs one adds it here explicitly. Only features
// Chromium recognizes are listed, so the header logs no parse warnings.
const PermissionsPolicy = "accelerometer=(), " +
	"bluetooth=(), " +
	"camera=(), " +
	"display-capture=(), " +
	"encrypted-media=(), " +
	"geolocation=(), " +
	"gyroscope=(), " +
	"hid=(), " +
	"idle-detection=(), " +
	"magnetometer=(), " +
	"microphone=(), " +
	"midi=(), " +
	"payment=(), " +
	"publickey-credentials-get=(), " +
	"screen-wake-lock=(), " +
	"serial=(), " +
	"usb=(), " +
	"xr-spatial-tracking=()"

// securityHeaders sets the baseline security headers on every response and a
// fresh CSP nonce per request. The nonce is stored in the request context
// with templ.WithNonce: templates read it with templ.GetNonce(ctx), and
// templ.JSONScript picks it up. HSTS is set by the gateway.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 128 bits from the CSPRNG, base32: valid nonce characters.
		nonce := rand.Text()

		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy(nonce))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", PermissionsPolicy)

		next.ServeHTTP(w, r.WithContext(templ.WithNonce(r.Context(), nonce)))
	})
}
