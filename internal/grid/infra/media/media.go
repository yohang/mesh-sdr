// Package media serves the node media WebSocket (GET /ws, rx.v1) reached
// only through the hub gateway (TECHNICAL_SPEC §5.8, §5.16, §6.2): it checks
// the gateway client certificate, verifies the access token offline with the
// keys received over the control channel, runs the session handshake, the
// in-band token refresh and the expiry, and closes connections revoked by
// the hub.
//
// Streaming (device attach, demodulators, FFT) comes with the DSP epics;
// device-scoped messages are checked against the token scope and answered
// unsupported_type.
package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/agent"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
)

// Header names set by the gateway after forward auth (§4.6 rule 5).
const (
	HeaderAccessToken = "X-Rx-Access-Token"
	HeaderCID         = "X-Rx-Cid"
)

// Connection limits (§6.9).
const (
	HelloTimeout   = 5 * time.Second
	MaxQueueBytes  = 4 << 20
	PingInterval   = 15 * time.Second
	PongTimeout    = 30 * time.Second
	RevokedMemory  = 15 * time.Minute
	defaultMsgRate = 20
)

// Problem codes of refused upgrades.
const (
	CodeHubUnavailable = "hub_unavailable"
	CodeOriginDenied   = "origin_denied"
	CodeTokenInvalid   = "token_invalid"
	CodeTokenExpired   = "token_expired"
	CodeConnectionUsed = "connection_in_use"
	CodeRevoked        = "revoked"
)

// Options configures a Server.
type Options struct {
	NodeID  string
	Version string
	// GatewayIdentity is the expected id in the gateway URI SAN (the hub
	// id); empty accepts any gateway certificate of the hub CA.
	GatewayIdentity string
	// OwnSerial returns the serial of the node certificate in use.
	OwnSerial func() string
	Agent     *agent.Agent
	Now       func() time.Time
	Logger    *slog.Logger
}

// Server is the /ws endpoint and the registry of its sessions.
type Server struct {
	o Options

	mu        sync.Mutex
	ctx       context.Context
	issuer    string
	origin    string
	keys      *token.KeySet
	withdrawn bool
	sessions  map[string]*session
	// used remembers every cid until its token can no longer be valid
	// (exp + leeway): a cid serves one connection only (§5.8).
	used     map[string]time.Time
	revSess  map[string]time.Time
	revUsers map[string]time.Time
}

// NewServer returns the server. Until Run binds it, upgrades are refused.
func NewServer(o Options) *Server {
	return &Server{o: o, sessions: map[string]*session{}, used: map[string]time.Time{}, revSess: map[string]time.Time{}, revUsers: map[string]time.Time{}}
}

// Run binds the sessions to ctx; when ctx is done every session is closed
// with 1001.
func (s *Server) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()

	<-ctx.Done()

	s.closeAll(rxv1.CloseGoingAway, "node shutting down")
}

// UpdateKeys implements control.Media.
func (s *Server) UpdateKeys(u ctl.KeysUpdate) error {
	keys, err := token.NewKeySet(token.JWKS{Keys: u.Keys}, u.RevokedKids)
	if err != nil {
		return fmt.Errorf("ctl.keys.update: %w", err)
	}

	origin, err := originOf(u.Issuer)
	if err != nil {
		return fmt.Errorf("ctl.keys.update issuer: %w", err)
	}

	s.mu.Lock()
	s.issuer, s.origin, s.keys, s.withdrawn = u.Issuer, origin, keys, false
	stale := []*session{}

	for _, ss := range s.sessions {
		if _, ok := keys.Key(ss.kid()); !ok {
			stale = append(stale, ss)
		}
	}
	s.mu.Unlock()

	for _, ss := range stale {
		ss.close(rxv1.CloseUnauthenticated, "signing key revoked")
	}

	return nil
}

// Revoke implements control.Media: sessions and users revoked by the hub
// are closed with 4403, and tokens issued before the revocation are refused
// for RevokedMemory. The node's own certificate serial closes everything.
func (s *Server) Revoke(r ctl.Revocations) {
	now := s.o.Now()

	if s.o.OwnSerial != nil && slices.Contains(r.CertSerials, s.o.OwnSerial()) {
		s.o.Logger.Error("this node's certificate was revoked by the hub: media connections refused until re-enrollment")
		s.Withdrawn()

		return
	}

	s.mu.Lock()
	keep(s.revSess, r.Sessions)
	keep(s.revUsers, r.Users)

	s.forgetRevocations(now)

	var hit []*session

	for _, ss := range s.sessions {
		if s.revokedLocked(ss.claims()) {
			hit = append(hit, ss)
		}
	}
	s.mu.Unlock()

	for _, ss := range hit {
		ss.close(rxv1.CloseForbidden, "revoked")
	}
}

// Withdrawn implements control.Media: the hub removed, disabled or revoked
// this node. Every session is closed with 4403 and the keys are dropped, so
// new connects are refused until a control channel installs keys again.
func (s *Server) Withdrawn() {
	s.mu.Lock()
	s.keys, s.withdrawn = nil, true
	s.mu.Unlock()

	s.closeAll(rxv1.CloseForbidden, "node withdrawn by the hub")
}

// Count returns the number of open sessions.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.sessions)
}

func (s *Server) closeAll(code rxv1.CloseCode, reason string) {
	s.mu.Lock()
	all := make([]*session, 0, len(s.sessions))

	for _, ss := range s.sessions {
		all = append(all, ss)
	}
	s.mu.Unlock()

	for _, ss := range all {
		ss.close(code, reason)
	}
}

func (s *Server) forgetRevocations(now time.Time) {
	for k, at := range s.revSess {
		if now.Sub(at) > RevokedMemory {
			delete(s.revSess, k)
		}
	}

	for k, at := range s.revUsers {
		if now.Sub(at) > RevokedMemory {
			delete(s.revUsers, k)
		}
	}
}

// revokedLocked reports whether claims were issued before a revocation of
// their session or user.
func (s *Server) revokedLocked(c token.Claims) bool {
	if at, ok := s.revSess[c.SessionID]; ok && c.SessionID != "" && !c.IssuedAt.After(at) {
		return true
	}

	if at, ok := s.revUsers[c.Subject]; ok && c.Subject != token.AnonymousSubject && !c.IssuedAt.After(at) {
		return true
	}

	return false
}

// forgetUsed forgets the cids whose tokens have all expired.
func (s *Server) forgetUsed(now time.Time) {
	for cid, until := range s.used {
		if now.After(until) {
			delete(s.used, cid)
		}
	}
}

// extendUsed keeps cid used until the end of a refreshed token.
func (s *Server) extendUsed(cid string, exp time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if until := exp.Add(token.Leeway); until.After(s.used[cid]) {
		s.used[cid] = until
	}
}

// keep records revocations with the hub time of each, never moving an
// entry to a later time.
func keep(known map[string]time.Time, revoked []ctl.Revoked) {
	for _, r := range revoked {
		at := time.UnixMilli(r.At)
		if first, ok := known[r.ID]; !ok || at.Before(first) {
			known[r.ID] = at
		}
	}
}

func originOf(issuer string) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid issuer %q", issuer)
	}

	return u.Scheme + "://" + u.Host, nil
}

func refuse(w http.ResponseWriter, status int, code, detail string) {
	problem.Write(w, problem.New(status, code, detail))
}

// verify checks raw for this node at now.
func (s *Server) verify(raw string) (token.Claims, error) {
	s.mu.Lock()
	keys, issuer := s.keys, s.issuer
	s.mu.Unlock()

	c, err := token.Verify(raw, keys, token.Expect{Issuer: issuer, Audience: token.Audience(s.o.NodeID), Now: s.o.Now()})
	if err != nil {
		return token.Claims{}, err
	}

	s.mu.Lock()
	revoked := s.revokedLocked(c)
	s.mu.Unlock()

	if revoked {
		return token.Claims{}, errRevoked
	}

	return c, nil
}

var errRevoked = errors.New("revoked")

// ServeHTTP implements http.Handler for GET /ws.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := s.o.Logger.With(slog.String("remote_addr", r.RemoteAddr))

	kind, id, ok := pki.PeerIdentity(r)
	if !ok || kind != pki.KindGateway || (s.o.GatewayIdentity != "" && id != s.o.GatewayIdentity) {
		log.WarnContext(r.Context(), "media connection refused: not the hub gateway", slog.String("peer_kind", kind), slog.String("peer_id", id))
		refuse(w, http.StatusForbidden, problem.CodeForbidden, "media connections are accepted from the hub gateway only")

		return
	}

	s.mu.Lock()
	base, keys, origin, withdrawn := s.ctx, s.keys, s.origin, s.withdrawn
	s.mu.Unlock()

	switch {
	case base == nil || keys == nil:
		reason := "no access-token keys received from the hub yet"
		if withdrawn {
			reason = "this node was withdrawn by the hub"
		}

		log.WarnContext(r.Context(), "media connection refused: "+reason)
		refuse(w, http.StatusServiceUnavailable, CodeHubUnavailable, reason)

		return
	case r.Header.Get("Origin") != origin:
		log.WarnContext(r.Context(), "media connection refused: origin", slog.String("origin", r.Header.Get("Origin")))
		refuse(w, http.StatusForbidden, CodeOriginDenied, "origin not allowed")

		return
	}

	claims, err := s.verify(r.Header.Get(HeaderAccessToken))

	switch {
	case errors.Is(err, token.ErrExpired):
		refuse(w, http.StatusUnauthorized, CodeTokenExpired, "access token expired")

		return
	case errors.Is(err, errRevoked):
		refuse(w, http.StatusForbidden, CodeRevoked, "access revoked")

		return
	case err != nil:
		log.WarnContext(r.Context(), "media connection refused: invalid access token", slog.Any("error", err))
		refuse(w, http.StatusUnauthorized, CodeTokenInvalid, "invalid access token")

		return
	case r.Header.Get(HeaderCID) != claims.ConnectionID:
		log.WarnContext(r.Context(), "media connection refused: cid does not match the token")
		refuse(w, http.StatusUnauthorized, CodeTokenInvalid, "connection id does not match the access token")

		return
	}

	ss := &session{s: s, cur: claims, logger: s.o.Logger.With(slog.String("cid", claims.ConnectionID))}

	s.mu.Lock()
	s.forgetUsed(s.o.Now())

	_, used := s.used[claims.ConnectionID]
	if !used {
		s.used[claims.ConnectionID] = claims.ExpiresAt.Add(token.Leeway)
		s.sessions[claims.ConnectionID] = ss
	}
	s.mu.Unlock()

	if used {
		refuse(w, http.StatusConflict, CodeConnectionUsed, "connection id already in use")

		return
	}

	defer func() {
		s.mu.Lock()
		delete(s.sessions, claims.ConnectionID)
		s.mu.Unlock()
	}()

	ws, err := wsconn.AcceptOriginChecked(w, r, rxv1.Subprotocol)
	if err != nil {
		log.DebugContext(r.Context(), "media upgrade failed", slog.Any("error", err))

		return
	}

	conn := wsconn.New(base, ws, wsconn.Options{
		ReadLimit: rxv1.MaxInboundTextBytes, MaxQueueBytes: MaxQueueBytes, PingInterval: PingInterval, PongTimeout: PongTimeout,
	})

	if !ss.attach(conn) {
		return
	}

	ss.run(base)
}
