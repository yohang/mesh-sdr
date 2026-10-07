package wire

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// events opens /api/ws through the gateway with the browser's cookies and
// origin, and runs the handshake; it returns the socket or the refusal.
func (b *hubBrowser) events(t *testing.T, origin string) (*websocket.Conn, int) {
	t.Helper()

	u, _ := url.Parse(b.base)
	h := http.Header{}

	if origin != "" {
		h.Set("Origin", origin)
	}

	for _, c := range b.c.Jar.Cookies(u) {
		h.Add("Cookie", c.Name+"="+c.Value)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(b.base, "http")+"/api/ws", &websocket.DialOptions{
		HTTPHeader: h, Subprotocols: []string{rxv1.Subprotocol},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		if resp == nil {
			t.Fatalf("dial: %v", err)
		}

		return nil, resp.StatusCode
	}

	t.Cleanup(func() { _ = ws.CloseNow() })

	wsSend(t, ws, rxv1.TypeSessionHello, "h1", map[string]any{"client": map[string]string{"name": "test", "version": "1"}})
	expectEnvelope(t, ws, rxv1.TypeSessionWelcome)

	return ws, http.StatusSwitchingProtocols
}

// sub subscribes and returns the ack or error type.
func sub(t *testing.T, ws *websocket.Conn, topics ...string) (rxv1.MessageType, map[string]any) {
	t.Helper()

	wsSend(t, ws, rxv1.TypeSub, "s", map[string]any{"topics": topics})

	env, err := readEnvelope(t, ws)
	if err != nil {
		t.Fatal(err)
	}

	var p map[string]any
	_ = json.Unmarshal(env.Payload(), &p)

	return env.Type(), p
}

// awaitEvent reads until an event of typ whose payload matches.
func awaitEvent(t *testing.T, ws *websocket.Conn, typ rxv1.MessageType, match func(map[string]any) bool) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		env, err := readEnvelope(t, ws)
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}

		var p map[string]any
		_ = json.Unmarshal(env.Payload(), &p)

		if env.Type() == typ && match(p) {
			return
		}
	}

	t.Fatalf("no %s event", typ)
}

// TestEventsEndToEnd runs the hub events WebSocket behind the gateway with
// a real node: origin check, topic access, node and device events,
// listener counts, presence rows and the end of a session (ADR 0016, ADR
// 0018).
func TestEventsEndToEnd(t *testing.T) {
	e := newGridEnvWith(t, true, fastTimings())
	newMediaUser(t, e)

	anon := e.anonymous(t)
	origin := anon.base

	if _, st := anon.events(t, "https://evil.example"); st != http.StatusForbidden {
		t.Fatalf("foreign origin = %d, want 403", st)
	}

	a, st := anon.events(t, origin)
	if a == nil {
		t.Fatalf("anonymous socket refused: %d", st)
	}

	// Without an anonymous-listenable device, anonymous visitors follow
	// nothing; admin topics are never theirs.
	if typ, p := sub(t, a, "nodes"); typ != rxv1.TypeError || p["code"] != "forbidden" {
		t.Fatalf("anonymous nodes before any device = %s %v", typ, p)
	}

	lis := e.signIn(t, "lis")

	ws, _ := lis.events(t, origin)
	if typ, p := sub(t, ws, "nodes", "devices", "presence"); typ != rxv1.TypeAck {
		t.Fatalf("listener sub = %s %v", typ, p)
	}

	if typ, p := sub(t, ws, "admin.connections"); typ != rxv1.TypeError || p["code"] != "forbidden" {
		t.Fatalf("listener admin topic = %s %v", typ, p)
	}

	e.enrollNode(t, fakeProber{})

	awaitEvent(t, ws, rxv1.TypeNodeStatus, func(p map[string]any) bool { return p["node_id"] == "attic" && p["status"] == "online" })
	awaitEvent(t, ws, rxv1.TypeDeviceStatus, func(p map[string]any) bool { return p["device_id"] == "hf" && p["node_id"] == "attic" })

	// The device is anonymous-listenable under the default policy: an
	// anonymous socket may now follow the nodes (a fresh one: a visitor's
	// socket without topics is closed after a grace period).
	_ = a.CloseNow()
	a, _ = anon.events(t, origin)

	if typ, p := sub(t, a, "nodes", "decodes:device=hf"); typ != rxv1.TypeAck {
		t.Fatalf("anonymous sub with a listenable device = %s %v", typ, p)
	}

	if typ, p := sub(t, a, "decodes:device=nope"); typ != rxv1.TypeError || p["code"] != "forbidden" {
		t.Fatalf("anonymous sub of an unknown device = %s %v", typ, p)
	}

	// A listener joining a device is counted on the presence topic.
	eventually(t, "keys on the node", 10*time.Second, func() bool { return e.g.manager.Connected(domain.MustNodeID("attic")) })

	media, _ := openMedia(t, lis)

	awaitEvent(t, ws, rxv1.TypePresenceCount, func(p map[string]any) bool { return p["total"] == 1.0 })

	_ = media.Close(websocket.StatusNormalClosure, "")

	awaitEvent(t, ws, rxv1.TypePresenceCount, func(p map[string]any) bool { return p["total"] == 0.0 })

	// Every socket has a presence row of kind events.
	repo := gridsqlite.NewConnectionRepository(e.adapter)
	if n, err := repo.CountOpenKind(context.Background(), domain.ConnectionEvents); err != nil || n != 2 {
		t.Fatalf("open events rows = %d, %v", n, err)
	}

	// Signing out ends the socket of that session at once.
	if st, body, _ := lis.form("/logout", nil); st != http.StatusNoContent {
		t.Fatalf("logout: %d %s", st, body)
	}

	awaitEvent(t, ws, rxv1.TypeSessionRevoked, func(map[string]any) bool { return true })
	expectWSClose(t, ws, rxv1.CloseUnauthenticated)

	eventually(t, "events row closed", 5*time.Second, func() bool {
		n, _ := repo.CountOpenKind(context.Background(), domain.ConnectionEvents)

		return n == 1
	})
}
