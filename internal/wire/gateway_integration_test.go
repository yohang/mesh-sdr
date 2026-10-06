package wire

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// browserDial opens /nodes/{id}/ws through the gateway as a browser would.
func (e *gridEnv) browserDial(t *testing.T, node string, h http.Header) (*websocket.Conn, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := websocket.Dial(ctx, "ws://"+e.gatewayAddr+"/nodes/"+node+"/ws", &websocket.DialOptions{
		HTTPHeader: h, Subprotocols: []string{rxv1.Subprotocol},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		if resp == nil {
			t.Fatal(err)
		}

		return nil, resp.StatusCode
	}

	t.Cleanup(func() { _ = ws.CloseNow() })

	return ws, http.StatusSwitchingProtocols
}

func readEnvelope(t *testing.T, ws *websocket.Conn) (rxv1.Envelope, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, b, err := ws.Read(ctx)
	if err != nil {
		return rxv1.Envelope{}, err
	}

	return rxv1.DecodeEnvelope(b)
}

// TestGatewayEndToEnd: browser WebSocket → gateway (forward auth, token
// injection) → node over mTLS, which verifies the token with the keys the
// hub pushed over the control channel (GRID-011, GRID-012, GRID-015).
func TestGatewayEndToEnd(t *testing.T) {
	e := newGridEnvWith(t, true, fastTimings())
	stopNode := e.enrollNode(t, fakeProber{})

	id := domain.MustNodeID("attic")
	ctx := context.Background()

	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(id) })
	eventually(t, "device registry", 5*time.Second, func() bool {
		_, err := e.g.devices.Get(ctx, "hf")

		return err == nil
	})

	origin := http.Header{}
	origin.Set("Origin", e.hubCfg.Hub.URL)

	if _, st := e.browserDial(t, "nowhere", origin); st != http.StatusNotFound {
		t.Fatalf("unknown node: %d", st)
	}

	if _, st := e.browserDial(t, "attic", http.Header{}); st != http.StatusForbidden {
		t.Fatalf("no Origin: %d", st)
	}

	h := origin.Clone()
	h.Set("X-Rx-Access-Token", "forged")
	h.Set("X-Rx-Cid", "forged")

	ws, st := e.browserDial(t, "attic", h)
	if st != http.StatusSwitchingProtocols {
		t.Fatalf("anonymous listener on an anonymous station: %d", st)
	}

	hello, _ := rxv1.NewEnvelope(rxv1.TypeSessionHello, rxv1.CorrelationID{}, time.Now().UnixMilli(),
		map[string]any{"client": map[string]string{"name": "e2e", "version": "1"}})
	b, _ := hello.MarshalJSON()

	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}

	env, err := readEnvelope(t, ws)
	if err != nil || env.Type() != rxv1.TypeSessionWelcome {
		t.Fatalf("welcome: %v %v", env.Type(), err)
	}

	var welcome struct {
		CID string `json:"cid"`
	}
	_ = json.Unmarshal(env.Payload(), &welcome)

	// The presence row created by the authz is confirmed by the node.
	eventually(t, "presence", 5*time.Second, func() bool {
		rows, err := e.g.presence.List(ctx)

		return err == nil && len(rows) == 1 && rows[0].Info().ID.String() == welcome.CID && rows[0].Info().NodeID == "attic"
	})

	// Removing the node closes its media connections and its route.
	if err := e.g.nodes.Delete(ctx, "cli", "attic"); err != nil {
		t.Fatal(err)
	}

	for {
		if _, err := readEnvelope(t, ws); err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(rxv1.CloseForbidden) {
				t.Fatalf("close status after removal = %d (%v), want 4403", got, err)
			}

			break
		}
	}

	if _, st := e.browserDial(t, "attic", origin); st != http.StatusNotFound {
		t.Fatalf("removed node: %d", st)
	}

	stopNode()
}

// TestGatewayNodeOffline: a node without its control channel gets 503.
func TestGatewayNodeOffline(t *testing.T) {
	e := newGridEnvWith(t, true, fastTimings())
	stop := e.enrollNode(t, fakeProber{})

	id := domain.MustNodeID("attic")
	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(id) })

	stop()

	origin := http.Header{}
	origin.Set("Origin", e.hubCfg.Hub.URL)

	eventually(t, "503", 10*time.Second, func() bool {
		_, st := e.browserDial(t, "attic", origin)

		return st == http.StatusServiceUnavailable
	})
}

// TestRevokedByAnotherProcess: a node revoked through the CLI while the hub
// runs (another process, no link to the control manager) is dropped by the
// next reconcile with the revocation list and 4403, so its media
// connections close (GRID-015).
func TestRevokedByAnotherProcess(t *testing.T) {
	e := newGridEnvWith(t, true, fastTimings(), func(o *control.HubOptions) { o.ReconcileEvery = 200 * time.Millisecond })
	e.enrollNode(t, fakeProber{})

	id := domain.MustNodeID("attic")

	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(id) })

	origin := http.Header{}
	origin.Set("Origin", e.hubCfg.Hub.URL)

	var ws *websocket.Conn

	eventually(t, "media connection", 5*time.Second, func() bool {
		var st int
		ws, st = e.browserDial(t, "attic", origin)

		return st == http.StatusSwitchingProtocols
	})

	cli, err := HubNodes(e.hubCfg, quiet, e.adapter)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := cli.Revoke(context.Background(), "cli", "attic"); err != nil {
		t.Fatal(err)
	}

	for {
		if _, err := readEnvelope(t, ws); err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(rxv1.CloseForbidden) {
				t.Fatalf("close status = %d (%v), want 4403", got, err)
			}

			break
		}
	}
}

// TestGatewayRevokedNode: revoking a node closes its media connections and
// the gateway answers 404 for it; the node row stays, revoked (GRID-015).
func TestGatewayRevokedNode(t *testing.T) {
	e := newGridEnvWith(t, true, fastTimings())
	e.enrollNode(t, fakeProber{})

	ctx := context.Background()
	id := domain.MustNodeID("attic")

	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(id) })

	origin := http.Header{}
	origin.Set("Origin", e.hubCfg.Hub.URL)

	var ws *websocket.Conn

	eventually(t, "media connection", 5*time.Second, func() bool {
		var st int
		ws, st = e.browserDial(t, "attic", origin)

		return st == http.StatusSwitchingProtocols
	})

	if _, err := e.g.nodes.Revoke(ctx, "cli", "attic"); err != nil {
		t.Fatal(err)
	}

	for {
		if _, err := readEnvelope(t, ws); err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(rxv1.CloseForbidden) {
				t.Fatalf("close status after revocation = %d (%v), want 4403", got, err)
			}

			break
		}
	}

	if _, st := e.browserDial(t, "attic", origin); st != http.StatusNotFound {
		t.Fatalf("revoked node: %d", st)
	}

	if n := e.node(t); n.Enrollment() != domain.EnrollmentRevoked {
		t.Fatalf("node = %+v", n.Snapshot())
	}
}
