// Package http serves the hub events WebSocket, GET /api/ws (TECHNICAL_SPEC
// §6.1, §6.2, §6.6, ADR 0016, ADR 0018): rx.v1 envelopes, session cookie
// (or anonymous), Origin check and socket caps at the upgrade, topic
// subscriptions authorised per topic, a presence row per socket, and the
// end of the socket when its session ends.
package http

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"

	"github.com/yohang/mesh-sdr/internal/events/app"
	"github.com/yohang/mesh-sdr/internal/events/domain"
	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Path is the hub events WS endpoint.
const Path = "/api/ws"

// Limits of the hub events WS (§6.9, ADR 0016).
const (
	MaxMessageBytes = 16 << 10
	MessageRate     = 10 // inbound messages per second
	// MaxQueueBytes bounds the pending outbound bytes of a connection
	// (ADR 0016 decision 12): beyond it the socket closes with 4413.
	MaxQueueBytes = 1 << 20
	// strikeLimit is the number of strikes allowed in strikeWindow; one
	// more closes the connection (§6.2 invalid_* → 4400; §5.9 forbidden
	// > 10/min → 4403, as on the node; ADR 0016 decision 12 rate_limited →
	// 4429).
	strikeLimit  = 10
	strikeWindow = time.Minute
)

// Problem codes of refused upgrades.
const (
	CodeSubprotocolRequired = "subprotocol_required"
	CodeOriginDenied        = "origin_denied"
)

// Identity is who opens a connection.
type Identity struct {
	// Viewer is the subscriber as audience predicates see it.
	Viewer domain.Viewer
	// UserID and SessionID are zero for an anonymous visitor.
	UserID    shared.UUID
	SessionID shared.UUID
	Name      string
	Roles     []string
	RoleRank  int
}

// Session reads the identity of the upgrade request.
type Session interface {
	// Identify describes the principal of ctx.
	Identify(ctx context.Context) Identity
	// Check re-reads the session of the upgrade request without recording
	// activity. It returns when the session ends at the latest (zero for
	// an anonymous request), or domain.ErrUnauthenticated once it ended.
	Check(ctx context.Context, r *http.Request) (time.Time, error)
}

// Timings of a connection; zero values take the defaults (§6.2, §6.9, §7.3).
type Timings struct {
	Hello time.Duration // session.hello deadline, 5 s
	// SessionCheck bounds the delay between two session checks (5 min): a
	// session also ends at its known expiry.
	SessionCheck time.Duration
	// MinSessionCheck is the shortest delay between two session checks
	// (5 s), even when the session's known expiry is closer or past.
	MinSessionCheck time.Duration
	Ping            time.Duration // WS ping period, 20 s
	Pong            time.Duration // pong timeout, 30 s
	Heartbeat       time.Duration // presence refresh of the live rows, 15 s
	// EmptyGrace is how long an anonymous socket may hold no topic before
	// it is closed (30 s): a visitor's socket must follow something.
	EmptyGrace time.Duration
}

func (t Timings) withDefaults() Timings {
	if t.Hello <= 0 {
		t.Hello = 5 * time.Second
	}

	if t.SessionCheck <= 0 {
		t.SessionCheck = 5 * time.Minute
	}

	if t.MinSessionCheck <= 0 {
		t.MinSessionCheck = 5 * time.Second
	}

	if t.Ping <= 0 {
		t.Ping = 20 * time.Second
	}

	if t.Pong <= 0 {
		t.Pong = 30 * time.Second
	}

	if t.Heartbeat <= 0 {
		t.Heartbeat = 15 * time.Second
	}

	if t.EmptyGrace <= 0 {
		t.EmptyGrace = 30 * time.Second
	}

	return t
}

// Deps are the dependencies of the module.
type Deps struct {
	Broker    *app.Broker
	Authz     app.Authorizer
	Session   Session
	Presence  app.Presence
	Admission *app.Admission
	// ClientIP returns the canonical client address of a request.
	ClientIP func(r *http.Request) string
	// Origin is the browser origin of the hub (scheme://host of hub.url).
	// An Origin header must be it, or name the request host (ADR 0016
	// decision 3); a request without Origin is not a browser's.
	Origin  string
	Version string
	Timings Timings
	Now     func() time.Time
	Logger  *slog.Logger
}

// Module is the router module (internal/http.Module) of /api/ws.
type Module struct {
	d        Deps
	shutdown chan struct{}
	once     sync.Once

	mu    sync.Mutex
	conns map[*conn]struct{}
}

// New returns the module.
func New(d Deps) *Module {
	d.Timings = d.Timings.withDefaults()
	if d.Now == nil {
		d.Now = time.Now
	}

	return &Module{d: d, shutdown: make(chan struct{}), conns: map[*conn]struct{}{}}
}

// Middlewares implements internal/http.Module.
func (m *Module) Middlewares() []func(http.Handler) http.Handler { return nil }

// Routes implements internal/http.Module. GET only: a WS upgrade is a GET,
// so the CSRF middleware lets it through; the Origin check of the upgrade
// is the cross-site defence (ADR 0003 §7, ADR 0016 decision 3).
func (m *Module) Routes(r chi.Router) { r.Get(Path, m.serve) }

// Run is a process worker. It refreshes the presence rows of the live
// sockets in one batch every heartbeat period, and when ctx is done it
// closes every socket with 1001 (http.Server.Shutdown does not track
// hijacked connections).
func (m *Module) Run(ctx context.Context) {
	t := time.NewTicker(m.d.Timings.Heartbeat)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			m.once.Do(func() { close(m.shutdown) })

			return
		case <-t.C:
			m.heartbeat(ctx)
		}
	}
}

// Open returns the number of open sockets.
func (m *Module) Open() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.conns)
}

func (m *Module) heartbeat(ctx context.Context) {
	m.mu.Lock()
	ids := make([]shared.UUID, 0, len(m.conns))

	for c := range m.conns {
		ids = append(ids, c.id)
	}
	m.mu.Unlock()

	if len(ids) == 0 {
		return
	}

	if err := m.d.Presence.Heartbeat(ctx, ids); err != nil {
		m.d.Logger.ErrorContext(ctx, "refresh events presence rows", slog.Int("count", len(ids)), slog.Any("error", err))
	}
}

// conn is one /api/ws connection.
type conn struct {
	m       *Module
	c       *wsconn.Conn
	r       *http.Request
	sub     *app.Subscription
	id      shared.UUID
	logger  *slog.Logger
	welcome atomic.Bool
	limiter *rate.Limiter
	strikes map[rxv1.CloseCode]*strikes
	// reason is the close reason recorded in the presence row.
	reason atomic.Value
	// watched and watchedAt are the last device recorded from
	// presence.heartbeat, and when (read loop only).
	watched   string
	watchedAt time.Time
}

// attachEvery is the shortest delay between two device records of one
// connection (presence.heartbeat).
const attachEvery = 10 * time.Second

func (cn *conn) closeReason() app.CloseReason {
	if r, ok := cn.reason.Load().(app.CloseReason); ok {
		return r
	}

	return app.CloseClient
}

// allowedOrigin reports whether the Origin of r may open the socket.
func (m *Module) allowedOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser: no cross-site risk
	}

	if m.d.Origin != "" && strings.EqualFold(origin, m.d.Origin) {
		return true
	}

	u, err := url.Parse(origin)

	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

func offers(r *http.Request) bool {
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(h, ",") {
			if strings.TrimSpace(p) == rxv1.Subprotocol {
				return true
			}
		}
	}

	return false
}

func (m *Module) refuse(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	m.d.Logger.WarnContext(r.Context(), "events ws upgrade refused", slog.Int("status", status), slog.String("code", code),
		slog.String("origin", r.Header.Get("Origin")))
	problem.Write(w, problem.New(status, code, detail))
}

func (m *Module) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	switch {
	case !offers(r):
		w.Header().Set("Sec-WebSocket-Protocol", rxv1.Subprotocol)
		m.refuse(w, r, http.StatusUpgradeRequired, CodeSubprotocolRequired, "the rx.v1 subprotocol is required")

		return
	case !m.allowedOrigin(r):
		m.refuse(w, r, http.StatusForbidden, CodeOriginDenied, "this origin may not open the events socket")

		return
	}

	who := m.d.Session.Identify(ctx)

	release, wait, err := m.d.Admission.Admit(who.Viewer.SessionRef, m.d.ClientIP(r), m.d.Now())
	if err != nil {
		p := problem.FromError(err)
		if wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		}

		m.refuse(w, r, p.Status, p.Code, p.Detail)

		return
	}
	defer release()

	id, err := shared.NewUUIDv7(m.d.Now())
	if err != nil {
		m.d.Logger.ErrorContext(ctx, "events ws connection id", slog.Any("error", err))
		problem.Write(w, problem.New(http.StatusInternalServerError, problem.CodeInternal, ""))

		return
	}

	ws, err := wsconn.AcceptOriginChecked(w, r, rxv1.Subprotocol)
	if err != nil {
		m.d.Logger.WarnContext(ctx, "events ws upgrade failed", slog.Any("error", err))

		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cn := &conn{
		m: m, r: r, id: id,
		logger:  m.d.Logger.With(slog.String("cid", id.String())),
		limiter: rate.NewLimiter(MessageRate, MessageRate),
		strikes: map[rxv1.CloseCode]*strikes{},
	}
	cn.c = wsconn.New(ctx, ws, wsconn.Options{
		ReadLimit: MaxMessageBytes, MaxQueueBytes: MaxQueueBytes,
		PingInterval: m.d.Timings.Ping, PongTimeout: m.d.Timings.Pong,
	})

	// §7.3 rule 1: the row exists before the first application frame.
	err = m.d.Presence.Open(ctx, app.Connection{
		ID: id, UserID: who.UserID, SessionID: who.SessionID, RoleRank: who.RoleRank,
		IP: m.d.ClientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		cn.logger.ErrorContext(ctx, "open events presence row", slog.Any("error", err))
		cn.c.Close(rxv1.CloseUnavailable, "hub unavailable")

		return
	}

	m.mu.Lock()
	m.conns[cn] = struct{}{}
	m.mu.Unlock()

	cn.sub = m.d.Broker.Attach(who.Viewer, m.d.Authz, cn.deliver)

	defer func() {
		cn.sub.Detach()

		m.mu.Lock()
		delete(m.conns, cn)
		m.mu.Unlock()

		reason := cn.closeReason()
		if err := m.d.Presence.Close(context.WithoutCancel(ctx), id, reason); err != nil {
			cn.logger.ErrorContext(ctx, "close events presence row", slog.Any("error", err))
		}

		cn.logger.DebugContext(ctx, "events ws closed", slog.String("reason", string(reason)), slog.Any("cause", cn.c.Err()))
	}()

	cn.logger.DebugContext(ctx, "events ws opened", slog.Bool("anonymous", who.Viewer.Anonymous()))

	hello := time.AfterFunc(m.d.Timings.Hello, func() {
		if !cn.welcome.Load() {
			cn.c.Close(rxv1.CloseHandshakeTimeout, "session.hello not received")
		}
	})
	defer hello.Stop()

	go func() {
		select {
		case <-m.shutdown:
			cn.reason.Store(app.CloseHubRestart)
			cn.c.Close(rxv1.CloseGoingAway, "hub shutting down")
		case <-cn.c.Done():
		}
	}()

	go cn.watch(ctx, !who.Viewer.Anonymous())

	cn.readLoop(ctx)
}

// deliver is the broker sink: it encodes the event and enqueues it. It
// never blocks; a full queue closes the connection with 4413 (wsconn).
func (cn *conn) deliver(ev app.Event) {
	typ, err := rxv1.ParseMessageType(ev.Type)
	if err == nil {
		err = rxv1.HubHubToClient().Check(typ)
	}

	if err != nil {
		cn.logger.Error("drop hub event of unknown type", slog.String("type", ev.Type), slog.Any("error", err))

		return
	}

	env, err := rxv1.NewEnvelope(typ, rxv1.CorrelationID{}, cn.m.d.Now().UnixMilli(), ev.Payload)
	if err != nil {
		cn.logger.Error("encode hub event", slog.String("type", ev.Type), slog.Any("error", err))

		return
	}

	if err := cn.c.Send(env); errors.Is(err, wsconn.ErrSlowConsumer) {
		cn.logger.Warn("events ws slow consumer, closed 4413")
	}
}

// watch ends the connection with its session and re-authorises the topics
// when asked: a session ended by identity (logout, revocation, role change,
// disabled user) is pushed through the broker; idle and absolute expiry
// are found by re-reading the session at its known expiry, at least every
// SessionCheck (ADR 0016 decision 4). The check records no activity.
func (cn *conn) watch(ctx context.Context, signedIn bool) {
	var check <-chan time.Time

	timer := time.NewTimer(time.Hour)
	timer.Stop()

	defer timer.Stop()

	schedule := func(until time.Time) {
		d := cn.m.d.Timings.SessionCheck
		if !until.IsZero() {
			d = min(d, max(until.Sub(cn.m.d.Now())+time.Second, cn.m.d.Timings.MinSessionCheck))
		}

		timer.Reset(d)
		check = timer.C
	}

	// An anonymous socket without topics is closed after a grace period:
	// it still counts against the caps meanwhile (ADR 0018).
	var empty <-chan time.Time

	if !signedIn {
		grace := time.NewTicker(cn.m.d.Timings.EmptyGrace)
		defer grace.Stop()

		empty = grace.C
	}

	if signedIn {
		until, err := cn.m.d.Session.Check(ctx, cn.r)
		if errors.Is(err, domain.ErrUnauthenticated) {
			cn.revoke("session_expired")

			return
		}

		if err != nil {
			cn.logger.ErrorContext(ctx, "check events ws session", slog.Any("error", err))
		}

		schedule(until)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-cn.c.Done():
			return
		case <-cn.sub.Ended():
			cn.revoke("session_revoked")

			return
		case <-cn.sub.Rechecks():
			if cn.recheck(ctx) {
				return
			}
		case <-empty:
			if len(cn.sub.Topics()) == 0 {
				cn.logger.DebugContext(ctx, "anonymous events ws closed without topics")
				cn.c.Close(rxv1.CloseNormal, "no topics")

				return
			}
		case <-check:
			until, err := cn.m.d.Session.Check(ctx, cn.r)

			switch {
			case errors.Is(err, domain.ErrUnauthenticated):
				cn.revoke("session_expired")

				return
			case err != nil:
				cn.logger.ErrorContext(ctx, "check events ws session", slog.Any("error", err))
			}

			schedule(until)
		}
	}
}

// recheck authorises the topics again; it reports whether the connection
// ended.
func (cn *conn) recheck(ctx context.Context) bool {
	dropped, err := cn.sub.Recheck(ctx)

	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		cn.revoke("session_expired")

		return true
	case err != nil:
		cn.logger.ErrorContext(ctx, "recheck events ws topics", slog.Any("error", err))

		return false
	}

	if len(dropped) > 0 {
		names := make([]string, len(dropped))
		for i, tp := range dropped {
			names[i] = tp.String()
		}

		cn.logger.DebugContext(ctx, "events ws topics dropped", slog.Any("topics", names))

		// §6.6 has no "unsubscribed by the server" message: an error frame
		// with re = null names the dropped topics (ADR 0016 decision 5).
		cn.sendError(nil, rxv1.ErrorPayload{
			Code: rxv1.CodeForbidden, Message: "Topics no longer allowed",
			Details: map[string]any{"topics": names},
		})
	}

	return false
}

// revoke tells the client its session ended, then closes with 4401.
func (cn *conn) revoke(reason string) {
	cn.reason.Store(app.ClosePolicy)
	cn.send(rxv1.TypeSessionRevoked, map[string]string{"reason": reason})
	cn.c.Close(rxv1.CloseUnauthenticated, "session ended")
}

func (cn *conn) readLoop(ctx context.Context) {
	for {
		env, err := cn.c.Read(ctx)
		if err != nil {
			var pe *rxv1.Error
			if !errors.As(err, &pe) {
				return // transport closed
			}

			cn.reject(pe.ID, pe, rxv1.CloseProtocolViolations)

			continue
		}

		id, _ := env.ID()

		if !cn.limiter.Allow() {
			retry := int64(1000 / MessageRate)
			cn.sendError(&id, rxv1.ErrorPayload{Code: rxv1.CodeRateLimited, Message: "Too many messages", Retryable: true, RetryAfterMS: &retry})
			cn.strike(rxv1.CloseRateLimited)

			continue
		}

		if err := rxv1.HubClientToHub().Check(env.Type()); err != nil {
			cn.reject(id, err, 0)

			continue
		}

		if !cn.welcome.Load() && env.Type() != rxv1.TypeSessionHello {
			cn.reject(id, &rxv1.Error{Code: rxv1.CodeInvalidEnvelope, Path: "type", Reason: "session.hello expected first"}, rxv1.CloseProtocolViolations)

			continue
		}

		switch env.Type() {
		case rxv1.TypeSessionHello:
			cn.hello(ctx, env, id)
		case rxv1.TypeSub:
			cn.subscribe(ctx, env, id)
		case rxv1.TypeUnsub:
			cn.unsubscribe(env, id)
		case rxv1.TypePresenceHeartbeat:
			cn.presence(ctx, env, id)
		default:
			cn.reject(id, &rxv1.Error{Code: rxv1.CodeUnsupportedType, Reason: "not handled by the hub"}, 0)
		}
	}
}

type helloPayload struct {
	Client struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"client"`
	Resume       *string         `json:"resume,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
}

func (cn *conn) hello(ctx context.Context, env rxv1.Envelope, id rxv1.CorrelationID) {
	if cn.welcome.Load() {
		cn.reject(id, &rxv1.Error{Code: rxv1.CodeInvalidEnvelope, Path: "type", Reason: "session already open"}, rxv1.CloseProtocolViolations)

		return
	}

	var p helloPayload
	if err := env.DecodePayload(&p, true); err != nil {
		cn.reject(id, err, rxv1.CloseProtocolViolations)

		return
	}

	who := cn.m.d.Session.Identify(ctx)

	roles := who.Roles
	if roles == nil {
		roles = []string{}
	}

	user := map[string]any{"roles": roles}
	if !who.Viewer.Anonymous() {
		user["id"], user["name"] = who.Viewer.UserID, who.Name
	}

	// No resume_token: the hub has nothing to resume, clients refetch
	// their fragments after (re)subscribing (ADR 0016 spec issue 2). No
	// token_exp: the hub WS relies on the session cookie.
	cn.send(rxv1.TypeSessionWelcome, map[string]any{
		"cid":         cn.id.String(),
		"server":      map[string]string{"product_version": cn.m.d.Version, "protocol": rxv1.Subprotocol},
		"user":        user,
		"limits":      map[string]int{"max_msg_bytes": MaxMessageBytes, "msg_rate": MessageRate, "max_topics": domain.MaxTopics},
		"server_time": cn.m.d.Now().UnixMilli(),
	})
	cn.welcome.Store(true)
}

type subPayload struct {
	Topics []string                   `json:"topics"`
	Since  map[string]json.RawMessage `json:"since,omitempty"`
}

func (cn *conn) subscribe(ctx context.Context, env rxv1.Envelope, id rxv1.CorrelationID) {
	var p subPayload
	if err := env.DecodePayload(&p, true); err != nil {
		cn.reject(id, err, rxv1.CloseProtocolViolations)

		return
	}

	topics, bad, err := cn.sub.Subscribe(ctx, p.Topics)
	if err != nil {
		cn.rejectDomain(id, bad, err)

		return
	}

	names := make([]string, len(topics))
	for i, t := range topics {
		names[i] = t.String()
	}

	// since cursors are ignored and no snapshot is sent: clients refetch
	// their fragments after the ack (ADR 0016 decision 9).
	cn.ack(id, map[string]any{"subscribed": names, "snapshots": map[string]any{}})
}

type unsubPayload struct {
	Topics []string `json:"topics"`
}

func (cn *conn) unsubscribe(env rxv1.Envelope, id rxv1.CorrelationID) {
	var p unsubPayload
	if err := env.DecodePayload(&p, true); err != nil {
		cn.reject(id, err, rxv1.CloseProtocolViolations)

		return
	}

	cn.sub.Unsubscribe(p.Topics)
	cn.ack(id, nil)
}

type presencePayload struct {
	View     string `json:"view"`
	DeviceID string `json:"device_id,omitempty"`
}

// view is the shape of a presence.heartbeat view ("receiver", "map", …).
var view = regexp.MustCompile(`^[a-z][a-z0-9_]{0,23}$`)

// presence records the device a viewer watches. Liveness comes from the
// socket itself (the hub refreshes the rows of its live sockets), so a
// heartbeat only carries the view (ADR 0018).
func (cn *conn) presence(ctx context.Context, env rxv1.Envelope, id rxv1.CorrelationID) {
	var p presencePayload
	if err := env.DecodePayload(&p, true); err != nil {
		cn.reject(id, err, rxv1.CloseProtocolViolations)

		return
	}

	if !view.MatchString(p.View) {
		cn.reject(id, &rxv1.Error{Code: rxv1.CodeInvalidPayload, Path: "view", Reason: "invalid view"}, rxv1.CloseProtocolViolations)

		return
	}

	if p.DeviceID != "" {
		// Watching a device needs listen permission on it, as its topics.
		t, err := domain.ParseTopic(string(domain.KindDecodes) + ":device=" + p.DeviceID)
		if err != nil {
			cn.reject(id, &rxv1.Error{Code: rxv1.CodeInvalidPayload, Path: "device_id", Reason: "invalid device id"}, rxv1.CloseProtocolViolations)

			return
		}

		if err := cn.m.d.Authz.AuthorizeTopic(ctx, t); err != nil {
			cn.rejectDomain(id, "", err)

			return
		}

		// One write per change of device, at most every attachEvery.
		if now := cn.m.d.Now(); p.DeviceID != cn.watched && now.Sub(cn.watchedAt) >= attachEvery {
			if err := cn.m.d.Presence.Attach(ctx, cn.id, p.DeviceID); err != nil {
				cn.logger.ErrorContext(ctx, "record watched device", slog.Any("error", err))
			} else {
				cn.watched, cn.watchedAt = p.DeviceID, now
			}
		}
	}

	cn.ack(id, nil)
}

// rejectDomain maps a subscription error to an error frame (ADR 0016
// conventions).
func (cn *conn) rejectDomain(id rxv1.CorrelationID, topic string, err error) {
	var de *shared.Error
	if !errors.As(err, &de) {
		cn.reject(id, err, 0)

		return
	}

	p := rxv1.ErrorPayload{Message: de.Message(), Details: map[string]any{}}
	if topic != "" {
		p.Details["topic"] = topic
	}

	escalate := rxv1.CloseCode(0)

	switch {
	case errors.Is(err, domain.ErrTopicForbidden), errors.Is(err, domain.ErrUnauthenticated):
		p.Code, escalate = rxv1.CodeForbidden, rxv1.CloseForbidden
	case errors.Is(err, domain.ErrTooManyTopics):
		// capacity_exceeded is defined for demodulators; reused (ADR 0016
		// spec issue 5).
		p.Code, p.Details = rxv1.CodeCapacityExceeded, map[string]any{"max_topics": domain.MaxTopics}
	default:
		p.Code, escalate = rxv1.CodeInvalidPayload, rxv1.CloseProtocolViolations
		p.Details["path"] = "topics"
	}

	cn.sendError(&id, p)

	if escalate != 0 {
		cn.strike(escalate)
	}
}

// reject answers err with an error frame and counts a strike toward
// escalate (0: no escalation).
func (cn *conn) reject(id rxv1.CorrelationID, err error, escalate rxv1.CloseCode) {
	incident := ""

	var pe *rxv1.Error
	if !errors.As(err, &pe) {
		incident = rand.Text()[:12]
		cn.logger.Error("events ws internal error", slog.String("incident_id", incident), slog.Any("error", err))
	}

	cn.sendError(&id, rxv1.ErrorPayloadFrom(err, incident))

	if escalate != 0 {
		cn.strike(escalate)
	}
}

func (cn *conn) sendError(id *rxv1.CorrelationID, p rxv1.ErrorPayload) {
	if id != nil && !id.IsZero() {
		re := *id
		p.Re = &re
	}

	env, err := rxv1.NewErrorEnvelope(cn.m.d.Now().UnixMilli(), p)
	if err != nil {
		cn.logger.Error("encode error frame", slog.Any("error", err))

		return
	}

	_ = cn.c.Send(env)
}

func (cn *conn) ack(id rxv1.CorrelationID, result any) {
	if id.IsZero() {
		return // fire-and-forget
	}

	env, err := rxv1.NewAckEnvelope(cn.m.d.Now().UnixMilli(), id, result)
	if err != nil {
		cn.logger.Error("encode ack", slog.Any("error", err))

		return
	}

	_ = cn.c.Send(env)
}

func (cn *conn) send(typ rxv1.MessageType, payload any) {
	env, err := rxv1.NewEnvelope(typ, rxv1.CorrelationID{}, cn.m.d.Now().UnixMilli(), payload)
	if err != nil {
		cn.logger.Error("encode envelope", slog.String("type", string(typ)), slog.Any("error", err))

		return
	}

	_ = cn.c.Send(env)
}

// strike counts a violation; more than strikeLimit strikes within
// strikeWindow close the connection with code. Read loop only.
func (cn *conn) strike(code rxv1.CloseCode) {
	s := cn.strikes[code]
	if s == nil {
		s = &strikes{}
		cn.strikes[code] = s
	}

	if s.add(cn.m.d.Now()) > strikeLimit {
		cn.logger.Warn("events ws closed after repeated violations", slog.Int("close_code", int(code)))
		cn.c.Close(code, code.String())
	}
}

// strikes is a sliding-window counter.
type strikes struct{ at []time.Time }

func (s *strikes) add(now time.Time) int {
	keep := s.at[:0]
	for _, t := range s.at {
		if now.Sub(t) < strikeWindow {
			keep = append(keep, t)
		}
	}

	s.at = append(keep, now)

	return len(s.at)
}
