package wire

import (
	"context"
	"crypto/tls"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// dialMedia opens the node /ws directly with the gateway certificate, as
// the gateway does after forward auth.
func (e *gridEnv) dialMedia(t *testing.T, raw, cid string) (*websocket.Conn, int) {
	t.Helper()

	cert, err := e.ca.MintClient(pki.KindGateway, "hub.example.org", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: e.ca.Pool(), ServerName: pki.NodeServerName("attic"),
		Certificates: []tls.Certificate{cert},
	}}}

	h := http.Header{}
	h.Set("Origin", "https://hub.example.org")
	h.Set("X-Rx-Access-Token", raw)
	h.Set("X-Rx-Cid", cid)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := websocket.Dial(ctx, "wss://"+e.nodeAddr+"/ws", &websocket.DialOptions{
		HTTPClient: client, HTTPHeader: h, Subprotocols: []string{rxv1.Subprotocol},
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

	return ws, resp.StatusCode
}

func (e *gridEnv) mediaToken(t *testing.T, cid string) string {
	t.Helper()

	now := time.Now()

	raw, err := e.g.keys.Issue(context.Background(), token.Claims{
		Issuer: e.hubCfg.Hub.URL, Audience: token.Audience("attic"), Subject: "u1", SessionID: token.SessionRef("s1"),
		ConnectionID: cid, Roles: []string{"listener"}, IssuedAt: now.Add(-time.Second), NotBefore: now, ExpiresAt: now.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// TestMediaKeysOverControl: the hub pushes its access-token keys over the
// control channel, so the node accepts the hub's tokens; revocations
// broadcast by the hub close the matching media connections.
func TestMediaKeysOverControl(t *testing.T) {
	e := newGridEnv(t, fastTimings())
	e.enrollNode(t, fakeProber{})

	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(domain.MustNodeID("attic")) })

	var ws *websocket.Conn

	eventually(t, "keys installed", 5*time.Second, func() bool {
		var st int
		ws, st = e.dialMedia(t, e.mediaToken(t, "c1"), "c1")

		return st == http.StatusSwitchingProtocols
	})

	e.g.manager.BroadcastRevocations(context.Background(), time.Time{}, nil, []string{"u1"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(rxv1.CloseForbidden) {
				t.Fatalf("close status = %d (%v), want 4403", got, err)
			}

			break
		}
	}

	// Removing the node withdraws it: the hub closes the control channel
	// with 4403 and the node refuses media connects.
	if err := e.g.nodes.Delete(context.Background(), "cli", "attic"); err != nil {
		t.Fatal(err)
	}

	eventually(t, "node withdrawn", 5*time.Second, func() bool {
		_, st := e.dialMedia(t, e.mediaToken(t, "c2"), "c2")

		return st == http.StatusServiceUnavailable || st == 0
	})
}

// TestMediaPresenceHeartbeats: a media session reports connection.heartbeat
// over the control channel, so its presence row outlives the stale delay,
// and is closed when the session ends (§7.3, GRID-017).
func TestMediaPresenceHeartbeats(t *testing.T) {
	timings := fastTimings()
	timings.PresenceStale = 1500 * time.Millisecond

	e := newGridEnv(t, timings)
	e.enrollNode(t, fakeProber{}, WithMediaHeartbeat(200*time.Millisecond))

	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(domain.MustNodeID("attic")) })

	cidUUID, err := shared.NewUUIDv7(time.Now())
	if err != nil {
		t.Fatal(err)
	}

	cid := cidUUID.String()

	var ws *websocket.Conn

	eventually(t, "keys installed", 5*time.Second, func() bool {
		var st int
		ws, st = e.dialMedia(t, e.mediaToken(t, cid), cid)

		return st == http.StatusSwitchingProtocols
	})

	repo := gridsqlite.NewConnectionRepository(e.adapter)
	open := func() bool {
		c, err := repo.Get(context.Background(), cidUUID)
		if err != nil {
			return false
		}

		_, _, closed := c.Closed()

		return !closed
	}

	eventually(t, "presence row opened", 5*time.Second, open)

	// Three stale delays later the row is still open: heartbeats keep it.
	time.Sleep(3 * timings.PresenceStale)

	if !open() {
		t.Fatal("the presence row of a live media session was reaped")
	}

	_ = ws.Close(websocket.StatusNormalClosure, "bye")

	eventually(t, "presence row closed", 5*time.Second, func() bool { return !open() })

	c, err := repo.Get(context.Background(), cidUUID)
	if err != nil {
		t.Fatal(err)
	}

	if _, reason, _ := c.Closed(); reason != domain.CloseClient {
		t.Fatalf("close reason = %s, want client", reason)
	}
}
