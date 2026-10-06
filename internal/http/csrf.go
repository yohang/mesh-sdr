package http

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"mime"
	"net/http"
	"sync"
)

// CSRFHeader carries the session-bound synchronizer token (TECHNICAL_SPEC §5.7).
const CSRFHeader = "X-CSRF-Token"

// SPIKE: cookie name for tls.mode=off (TECHNICAL_SPEC §5.6); with TLS it is
// __Host-rx_session with Secure. Real sessions come with AUTH-003/AUTH-004.
const sessionCookie = "rx_session"

// session is the slice of a session the shell needs: its id and its CSRF secret.
type session struct {
	id         string
	csrfSecret []byte
}

// csrfToken is HMAC-SHA256(csrf_secret, session id), base64url (TECHNICAL_SPEC §5.7).
func (s session) csrfToken() string {
	mac := hmac.New(sha256.New, s.csrfSecret)
	mac.Write([]byte(s.id))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s session) validCSRFToken(token string) bool {
	return token != "" && hmac.Equal([]byte(token), []byte(s.csrfToken()))
}

type sessionKey struct{}

func sessionFrom(ctx context.Context) (session, bool) {
	s, ok := ctx.Value(sessionKey{}).(session)
	return s, ok
}

// fakeSessions stands in for the DB-backed session store.
//
// SPIKE: in-memory, unbounded, never expires, gives every visitor a session (it plays
// the role of both the real session and the anonymous pre-session of §5.7). The real
// store hashes the id (sessions.id_hash) and persists csrf_secret.
type fakeSessions struct {
	mu      sync.Mutex
	secrets map[string][]byte
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{secrets: map[string][]byte{}}
}

func (f *fakeSessions) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := f.lookup(r)
		if !ok {
			s = f.create()
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookie,
				Value:    s.id,
				Path:     "/",
				HttpOnly: true,
				Secure:   r.TLS != nil,
				SameSite: http.SameSiteLaxMode,
			})
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)))
	})
}

func (f *fakeSessions) lookup(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	secret, ok := f.secrets[c.Value]
	if !ok {
		return session{}, false
	}

	return session{id: c.Value, csrfSecret: secret}, true
}

func (f *fakeSessions) create() session {
	s := session{id: randomToken(32), csrfSecret: []byte(randomToken(32))}

	f.mu.Lock()
	f.secrets[s.id] = s.csrfSecret
	f.mu.Unlock()

	return s
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never returns an error (crypto/rand, Go ≥ 1.24)

	return base64.RawURLEncoding.EncodeToString(b)
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// csrfProtection enforces, on every unsafe method:
//  1. net/http CrossOriginProtection (Sec-Fetch-Site / Origin vs Host), defence in depth;
//  2. the session-bound synchronizer token in the X-CSRF-Token header.
//
// A failure answers 403 and is logged at Warn (the request is handled, not an error).
func csrfProtection(logger *slog.Logger) func(http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	// SPIKE: AddTrustedOrigin(hub.url + gateway.extra_origins) once that config exists.

	return func(next http.Handler) http.Handler {
		deny := func(w http.ResponseWriter, r *http.Request, reason string) {
			logger.LogAttrs(r.Context(), slog.LevelWarn, "csrf check failed",
				slog.String("reason", reason),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("origin", r.Header.Get("Origin")),
				slog.String("sec_fetch_site", r.Header.Get("Sec-Fetch-Site")),
			)
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			if err := cop.Check(r); err != nil {
				deny(w, r, "cross_origin")
				return
			}

			s, ok := sessionFrom(r.Context())
			if !ok || !s.validCSRFToken(r.Header.Get(CSRFHeader)) {
				deny(w, r, "invalid_token")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// requireJSON rejects unsafe requests whose body is not application/json
// (TECHNICAL_SPEC §5.7, AUTH-019): a cross-site HTML form cannot send that type
// without a CORS preflight.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}
