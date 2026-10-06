package http

import "net/http"

// robotsTxt keeps crawlers away from the session, admin, API and node paths
// (UI-005).
const robotsTxt = `User-agent: *
Disallow: /login
Disallow: /logout
Disallow: /admin
Disallow: /api/
Disallow: /nodes/
`

func robots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(robotsTxt))
}
