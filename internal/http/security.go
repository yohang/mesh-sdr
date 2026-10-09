package http

import (
	"crypto/rand"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/a-h/templ"

	"github.com/yohang/mesh-sdr/internal/web"
)

// ContentSecurityPolicy returns the Content-Security-Policy sent with every
// response (TECHNICAL_SPEC §2.3, ADR 0003 §3):
//
//   - script-src: the per-request nonce, no 'self' and no 'unsafe-eval'.
//     A same-origin file (for example a Files upload) cannot run as a script,
//     and htmx features that evaluate JS (hx-on, js: expressions) are blocked.
//     Modules imported by a nonce'd entry module inherit its nonce. A worklet
//     request carries no nonce, so scripts lists the exact absolute URLs of
//     the static worklet files allowed besides (ADR 0015 decision 5).
//   - style-src 'self': no inline <style> nor style="" attributes. htmx 4
//     injects its indicator CSS as a constructable stylesheet, which this
//     does not block.
//   - img-src 'self' data:, plus the origins of images, the HTTPS origins
//     of the map tiles the browser loads directly (MAP-004, ADR 0029).
//     Boosted navigation keeps the first document, so every page carries
//     them.
//   - connect-src 'self': fetch, htmx and same-origin WebSockets.
func ContentSecurityPolicy(nonce string, scripts, images []string) string {
	src := "'nonce-" + nonce + "'"
	if len(scripts) > 0 {
		src += " " + strings.Join(scripts, " ")
	}

	img := "'self' data:"
	if len(images) > 0 {
		img += " " + strings.Join(images, " ")
	}

	return "default-src 'none'; " +
		"script-src " + src + "; " +
		"style-src 'self'; " +
		"img-src " + img + "; " +
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

// WorkletScripts returns the script URLs the CSP allows besides the nonce:
// the receiver audio worklet (web.ReceiverWorkletPath) on the origin of the
// hub public URL. An empty or invalid URL allows none.
func WorkletScripts(publicURL string) []string {
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}

	return []string{u.Scheme + "://" + u.Host + web.ReceiverWorkletPath}
}

// ImageSources is implemented by a Module whose pages load images from
// other origins (the map tiles): the CSP img-src lists them besides 'self'.
// It is called on every response, so its answer follows the settings.
type ImageSources interface {
	ImageSources() []string
}

// imageOrigin is an origin img-src may list: HTTPS, a host name, no port,
// path nor wildcard.
var imageOrigin = regexp.MustCompile(`^https://[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// imageSources returns the valid, distinct origins of sources; the others
// are dropped, so a bad value can never widen or break the policy.
func imageSources(sources []ImageSources) []string {
	var out []string

	for _, s := range sources {
		for _, o := range s.ImageSources() {
			if imageOrigin.MatchString(o) && !slices.Contains(out, o) {
				out = append(out, o)
			}
		}
	}

	return out
}

// securityHeaders sets the baseline security headers on every response and a
// fresh CSP nonce per request. The nonce is stored in the request context
// with templ.WithNonce: templates read it with templ.GetNonce(ctx), and
// templ.JSONScript picks it up. HSTS is set by the gateway.
func securityHeaders(scripts []string, images []ImageSources) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 128 bits from the CSPRNG, base32: valid nonce characters.
			nonce := rand.Text()

			h := w.Header()
			h.Set("Content-Security-Policy", ContentSecurityPolicy(nonce, scripts, imageSources(images)))
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Permissions-Policy", PermissionsPolicy)

			next.ServeHTTP(w, r.WithContext(templ.WithNonce(r.Context(), nonce)))
		})
	}
}
