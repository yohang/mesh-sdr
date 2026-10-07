package events_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/events"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

const hubOrigin = "https://hub.example.org"

// session is a signed-in user whose session can end or expire.
type session struct {
	mu     sync.Mutex
	anon   bool
	ended  bool
	until  time.Time
	checks int
}

func (s *session) Identify(context.Context) events.Identity {
	s.mu.Lock()
	anon := s.anon
	s.mu.Unlock()

	if anon {
		return events.Identity{}
	}

	return events.Identity{
		Viewer: events.Viewer{UserID: "u1", SessionRef: "ref1"}, Name: "alice", Roles: []string{"listener"}, RoleRank: 1,
		UserID: shared.MustParseUUID("01890000-0000-7000-8000-000000000001"), SessionID: shared.MustParseUUID("01890000-0000-7000-8000-000000000002"),
	}
}

func (s *session) Check(context.Context, *http.Request) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.checks++

	if s.ended {
		return time.Time{}, events.ErrUnauthenticated
	}

	return s.until, nil
}

func (s *session) end() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
}

// wsAuthz denies admin topics and the topics of device "secret".
type wsAuthz struct{}

func (wsAuthz) AuthorizeTopic(_ context.Context, t events.Topic) error {
	if t.IsAdmin() || t.Device() == "secret" {
		return events.ErrTopicForbidden
	}

	return nil
}

// presence records the registry calls.
type presence struct {
	mu       sync.Mutex
	open     map[shared.UUID]events.Connection
	closed   map[shared.UUID]events.CloseReason
	beats    int
	attached map[shared.UUID]string
	attaches int
}

func newPresence() *presence {
	return &presence{open: map[shared.UUID]events.Connection{}, closed: map[shared.UUID]events.CloseReason{}, attached: map[shared.UUID]string{}}
}

func (p *presence) Open(_ context.Context, c events.Connection) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.open[c.ID] = c

	return nil
}

func (p *presence) Heartbeat(_ context.Context, ids []shared.UUID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.beats += len(ids)

	return nil
}

func (p *presence) Attach(_ context.Context, id shared.UUID, device string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.attached[id] = device
	p.attaches++

	return nil
}

func (p *presence) Close(_ context.Context, id shared.UUID, reason events.CloseReason) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed[id] = reason

	return nil
}

func (p *presence) reasons() []events.CloseReason {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := []events.CloseReason{}
	for _, r := range p.closed {
		out = append(out, r)
	}

	return out
}

type fixture struct {
	srv      *httptest.Server
	broker   *events.Broker
	session  *session
	presence *presence
	module   *events.Module
	stop     context.CancelFunc
}

func newFixture(t *testing.T, timings events.Timings, limits ...events.Limits) *fixture {
	t.Helper()

	lim := events.DefaultLimits()
	if len(limits) > 0 {
		lim = limits[0]
	}

	f := &fixture{broker: events.NewBroker(), session: &session{}, presence: newPresence()}
	f.module = events.New(events.Deps{
		Broker: f.broker, Authz: wsAuthz{}, Session: f.session, Presence: f.presence, Admission: events.NewAdmission(lim),
		ClientIP: func(r *http.Request) string { h, _, _ := net.SplitHostPort(r.RemoteAddr); return h },
		Origin:   hubOrigin, Version: "test", Timings: timings, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	r := chi.NewRouter()
	f.module.Routes(r)

	f.srv = httptest.NewServer(r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { f.module.Run(ctx); close(done) }()

	f.stop = func() { cancel(); <-done }

	t.Cleanup(func() {
		cancel()
		<-done
		f.srv.Close()
	})

	return f
}

func (f *fixture) url() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") + events.Path }

// try dials and returns the connection, or the refusal status.
func (f *fixture) try(t *testing.T, origin string, protocols ...string) (*websocket.Conn, *http.Response) {
	t.Helper()

	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}

	if protocols == nil {
		protocols = []string{rxv1.Subprotocol}
	}

	ws, resp, err := websocket.Dial(context.Background(), f.url(), &websocket.DialOptions{Subprotocols: protocols, HTTPHeader: h})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		if resp == nil {
			t.Fatalf("dial: %v", err)
		}

		return nil, resp
	}

	t.Cleanup(func() { _ = ws.CloseNow() })

	return ws, resp
}

func (f *fixture) dial(t *testing.T, origin string) *websocket.Conn {
	t.Helper()

	ws, resp := f.try(t, origin)
	if ws == nil {
		t.Fatalf("refused: %d", resp.StatusCode)
	}

	return ws
}

func send(t *testing.T, ws *websocket.Conn, typ, id string, payload any) {
	t.Helper()

	env := map[string]any{"v": 1, "type": typ, "ts": time.Now().UnixMilli(), "payload": payload}
	if id != "" {
		env["id"] = id
	}

	b, _ := json.Marshal(env)
	if err := ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
}

type frame struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

func recv(t *testing.T, ws *websocket.Conn) frame {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, b, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var f frame
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}

	return f
}

func expectClose(t *testing.T, ws *websocket.Conn, want rxv1.CloseCode) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for {
		_, _, err := ws.Read(ctx)
		if err == nil {
			continue
		}

		if code := websocket.CloseStatus(err); code != websocket.StatusCode(want) {
			t.Fatalf("close = %v (%v), want %d", code, err, want)
		}

		return
	}
}

func open(t *testing.T, f *fixture) *websocket.Conn {
	t.Helper()

	ws := f.dial(t, "")
	send(t, ws, "session.hello", "h1", map[string]any{"client": map[string]any{"name": "test", "version": "1"}})

	w := recv(t, ws)
	if w.Type != "session.welcome" || w.Payload["user"].(map[string]any)["name"] != "alice" {
		t.Fatalf("welcome = %+v", w)
	}

	if _, ok := w.Payload["resume_token"]; ok {
		t.Errorf("welcome carries a resume token: %+v", w.Payload)
	}

	return ws
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestUpgradeChecks(t *testing.T) {
	f := newFixture(t, events.Timings{})

	// No subprotocol: 426 problem+json (§6.1, ADR 0013).
	if _, resp := f.try(t, "", "other"); resp.StatusCode != http.StatusUpgradeRequired ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("no subprotocol: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// A foreign origin is refused (403); hub.url and the request host are
	// accepted, as is a request without Origin (not a browser).
	if _, resp := f.try(t, "https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", resp.StatusCode)
	}

	if _, resp := f.try(t, "https://hub.example.org.evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lookalike origin: %d", resp.StatusCode)
	}

	_ = f.dial(t, f.srv.URL)
	_ = f.dial(t, hubOrigin)
	_ = f.dial(t, "")
}

func TestSocketCaps(t *testing.T) {
	f := newFixture(t, events.Timings{}, events.Limits{PerSession: 2, PerAddress: 10, Total: 10, UpgradesPerMinute: 3})

	a := f.dial(t, "")
	_ = f.dial(t, "")

	// A third socket of the session is refused with 429.
	if _, resp := f.try(t, ""); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third socket: %d", resp.StatusCode)
	}

	// Closing one frees its slot, but the address used its 3 upgrades of
	// the minute: 429 with Retry-After.
	_ = a.Close(websocket.StatusNormalClosure, "")

	eventually(t, "slot released", func() bool { return f.module.Open() == 1 })

	_, resp := f.try(t, "")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("upgrade beyond the rate: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestSubscribeAndReceive(t *testing.T) {
	f := newFixture(t, events.Timings{})
	ws := open(t, f)

	send(t, ws, "sub", "s1", map[string]any{"topics": []string{"nodes"}})

	ack := recv(t, ws)
	if ack.Type != "ack" || ack.Payload["re"] != "s1" {
		t.Fatalf("ack = %+v", ack)
	}

	f.broker.Publish(context.Background(), events.Event{
		Topic: events.MustTopic("nodes"), Type: "node.status",
		Payload: map[string]string{"node_id": "n1", "status": "offline"},
	})
	f.broker.Publish(context.Background(), events.Event{Topic: events.MustTopic("devices"), Type: "device.status"})

	ev := recv(t, ws)
	if ev.Type != "node.status" || ev.Payload["node_id"] != "n1" {
		t.Fatalf("event = %+v", ev)
	}

	// Forbidden topic: error frame, nothing subscribed (all-or-nothing).
	send(t, ws, "sub", "s2", map[string]any{"topics": []string{"devices", "admin.connections"}})

	e := recv(t, ws)
	if e.Type != "error" || e.Payload["code"] != "forbidden" || e.Payload["re"] != "s2" {
		t.Fatalf("error = %+v", e)
	}

	f.broker.Publish(context.Background(), events.Event{Topic: events.MustTopic("devices"), Type: "device.status"})
	f.broker.Publish(context.Background(), events.Event{Topic: events.MustTopic("nodes"), Type: "node.status", Payload: map[string]string{}})

	if ev := recv(t, ws); ev.Type != "node.status" {
		t.Fatalf("a refused sub subscribed devices: %+v", ev)
	}

	// Unknown topic and unknown payload field: invalid_payload.
	send(t, ws, "sub", "s3", map[string]any{"topics": []string{"bogus"}})

	if e := recv(t, ws); e.Payload["code"] != "invalid_payload" {
		t.Fatalf("error = %+v", e)
	}

	send(t, ws, "sub", "s4", map[string]any{"topics": []string{"nodes"}, "extra": true})

	if e := recv(t, ws); e.Payload["code"] != "invalid_payload" {
		t.Fatalf("error = %+v", e)
	}

	// A media message type is not accepted here.
	send(t, ws, "demod.set", "x", map[string]any{})

	if e := recv(t, ws); e.Payload["code"] != "unsupported_type" {
		t.Fatalf("error = %+v", e)
	}
}

func TestPresenceRows(t *testing.T) {
	f := newFixture(t, events.Timings{Heartbeat: 20 * time.Millisecond})
	ws := open(t, f)

	f.presence.mu.Lock()
	n := len(f.presence.open)
	var c events.Connection
	for _, c = range f.presence.open {
	}
	f.presence.mu.Unlock()

	if n != 1 || c.RoleRank != 1 || c.UserID.IsZero() || c.IP != "127.0.0.1" {
		t.Fatalf("presence rows = %d %+v", n, c)
	}

	eventually(t, "batched heartbeats", func() bool {
		f.presence.mu.Lock()
		defer f.presence.mu.Unlock()

		return f.presence.beats >= 2
	})

	// presence.heartbeat records a listenable device, refuses another.
	send(t, ws, "presence.heartbeat", "p1", map[string]any{"view": "receiver", "device_id": "hf"})

	if a := recv(t, ws); a.Type != "ack" {
		t.Fatalf("heartbeat = %+v", a)
	}

	send(t, ws, "presence.heartbeat", "p2", map[string]any{"view": "receiver", "device_id": "secret"})

	if e := recv(t, ws); e.Payload["code"] != "forbidden" {
		t.Fatalf("heartbeat on a forbidden device = %+v", e)
	}

	// The same device again, or another one within 10 s: no write.
	send(t, ws, "presence.heartbeat", "p3", map[string]any{"view": "receiver", "device_id": "hf"})
	recv(t, ws)
	send(t, ws, "presence.heartbeat", "p4", map[string]any{"view": "receiver", "device_id": "vhf"})
	recv(t, ws)

	f.presence.mu.Lock()
	attached, writes := f.presence.attached[c.ID], f.presence.attaches
	f.presence.mu.Unlock()

	if attached != "hf" || writes != 1 {
		t.Errorf("attached device = %q after %d writes, want hf after 1", attached, writes)
	}

	_ = ws.Close(websocket.StatusNormalClosure, "")

	eventually(t, "row closed", func() bool { return len(f.presence.reasons()) == 1 })

	if r := f.presence.reasons()[0]; r != events.CloseClient {
		t.Errorf("close reason = %s", r)
	}
}

func TestHelloRequiredFirst(t *testing.T) {
	f := newFixture(t, events.Timings{})
	ws := f.dial(t, "")

	send(t, ws, "sub", "s1", map[string]any{"topics": []string{"nodes"}})

	if e := recv(t, ws); e.Type != "error" || e.Payload["code"] != "invalid_envelope" {
		t.Fatalf("error = %+v", e)
	}
}

func TestHelloTimeoutCloses4408(t *testing.T) {
	f := newFixture(t, events.Timings{Hello: 50 * time.Millisecond})
	ws := f.dial(t, "")

	expectClose(t, ws, rxv1.CloseHandshakeTimeout)
}

// TestRevokedSession: a session ended by identity is pushed at once.
func TestRevokedSession(t *testing.T) {
	f := newFixture(t, events.Timings{})
	ws := open(t, f)

	f.broker.EndSessions([]string{"ref1"}, nil)

	if ev := recv(t, ws); ev.Type != "session.revoked" || ev.Payload["reason"] != "session_revoked" {
		t.Fatalf("event = %+v", ev)
	}

	expectClose(t, ws, rxv1.CloseUnauthenticated)
	eventually(t, "row closed", func() bool { return len(f.presence.reasons()) == 1 })

	if r := f.presence.reasons()[0]; r != events.ClosePolicy {
		t.Errorf("close reason = %s, want policy", r)
	}
}

// TestExpiredSession: a session is re-read at its known expiry; an expired
// one ends the socket.
func TestExpiredSession(t *testing.T) {
	f := newFixture(t, events.Timings{SessionCheck: time.Hour, MinSessionCheck: 200 * time.Millisecond})
	f.session.until = time.Now().Add(-time.Second) // re-read at the floor

	ws := open(t, f)

	f.session.end()

	if ev := recv(t, ws); ev.Type != "session.revoked" || ev.Payload["reason"] != "session_expired" {
		t.Fatalf("event = %+v", ev)
	}

	expectClose(t, ws, rxv1.CloseUnauthenticated)
}

// TestDroppedTopics: a recheck drops the topics now denied.
func TestDroppedTopics(t *testing.T) {
	f := newFixture(t, events.Timings{})
	ws := open(t, f)

	send(t, ws, "sub", "s1", map[string]any{"topics": []string{"nodes"}})
	recv(t, ws)

	// authz never denies "nodes": a recheck drops nothing, sends nothing.
	f.broker.RecheckAll()
	f.broker.Publish(context.Background(), events.Event{Topic: events.MustTopic("nodes"), Type: "node.status", Payload: map[string]string{}})

	if ev := recv(t, ws); ev.Type != "node.status" {
		t.Fatalf("after recheck = %+v", ev)
	}
}

func TestShutdownCloses1001(t *testing.T) {
	f := newFixture(t, events.Timings{})
	ws := open(t, f)

	f.stop()

	expectClose(t, ws, rxv1.CloseGoingAway)
	eventually(t, "row closed", func() bool { return len(f.presence.reasons()) == 1 })

	if r := f.presence.reasons()[0]; r != events.CloseHubRestart {
		t.Errorf("close reason = %s, want hub_restart", r)
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t, events.Timings{})
	ws := open(t, f)

	for range events.MessageRate + 5 {
		send(t, ws, "presence.heartbeat", "", map[string]any{"view": "map"})
	}

	// Fire-and-forget heartbeats get no ack; the excess gets rate_limited.
	e := recv(t, ws)
	if e.Payload["code"] != "rate_limited" || e.Payload["retry_after_ms"] == nil {
		t.Fatalf("error = %+v", e)
	}
}

func TestRepeatedForbiddenCloses4403(t *testing.T) {
	f := newFixture(t, events.Timings{})
	ws := open(t, f)

	// Spread under the message rate: ten refusals keep the connection.
	for i := range 10 {
		send(t, ws, "sub", "f", map[string]any{"topics": []string{"admin.connections"}})

		if e := recv(t, ws); e.Payload["code"] != "forbidden" {
			t.Fatalf("refusal %d = %+v", i, e)
		}

		time.Sleep(110 * time.Millisecond)
	}

	send(t, ws, "sub", "ok", map[string]any{"topics": []string{"nodes"}})

	if a := recv(t, ws); a.Type != "ack" {
		t.Fatalf("after ten refusals = %+v", a)
	}

	// The eleventh closes it.
	send(t, ws, "sub", "f", map[string]any{"topics": []string{"admin.connections"}})
	recv(t, ws)
	expectClose(t, ws, rxv1.CloseForbidden)
}

// TestSessionCheckFloor: a session whose known expiry is past is not
// re-read in a loop: the checks keep the floor delay.
func TestSessionCheckFloor(t *testing.T) {
	f := newFixture(t, events.Timings{SessionCheck: time.Hour})
	f.session.until = time.Now().Add(-time.Minute)

	_ = open(t, f)

	time.Sleep(time.Second)

	f.session.mu.Lock()
	checks := f.session.checks
	f.session.mu.Unlock()

	if checks > 1 {
		t.Errorf("%d session checks in a second, want the first only", checks)
	}
}

// TestAnonymousWithoutTopics: an anonymous socket that follows nothing is
// closed after the grace period; one with topics stays open.
func TestAnonymousWithoutTopics(t *testing.T) {
	f := newFixture(t, events.Timings{EmptyGrace: 200 * time.Millisecond})
	f.session.anon = true

	idle := f.dial(t, "")
	busy := f.dial(t, "")

	for _, ws := range []*websocket.Conn{idle, busy} {
		send(t, ws, "session.hello", "h", map[string]any{"client": map[string]any{"name": "test", "version": "1"}})
		recv(t, ws)
	}

	send(t, busy, "sub", "s", map[string]any{"topics": []string{"nodes"}})
	recv(t, busy)

	expectClose(t, idle, rxv1.CloseNormal)

	time.Sleep(500 * time.Millisecond)

	send(t, busy, "unsub", "u", map[string]any{"topics": []string{"presence"}})

	if a := recv(t, busy); a.Type != "ack" {
		t.Fatalf("socket with topics = %+v", a)
	}
}
