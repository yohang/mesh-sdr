package control_test

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/agent"
	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
)

var discard = slog.New(slog.DiscardHandler)

type prober struct{}

func (prober) Capabilities(context.Context) ctl.Capabilities {
	return ctl.Capabilities{ProductVersion: "dev", Protocols: []string{"rx-ctl.v1"}}
}

func (prober) Heartbeat(context.Context) ctl.Heartbeat { return ctl.Heartbeat{UptimeS: 1} }

func (prober) NTPSynced() bool { return true }

type node struct {
	url   string
	ca    *pki.CA
	agent *agent.Agent
}

func startNode(t *testing.T, helloTimeout time.Duration) *node {
	t.Helper()

	return startNodeWith(t, helloTimeout, 0, nil)
}

func startNodeWith(t *testing.T, helloTimeout time.Duration, queueBytes int, prefill func(*agent.Agent)) *node {
	t.Helper()

	certPEM, keyPEM, _ := pki.GenerateCA("hub", time.Now())
	ca, _ := pki.ParseCA(certPEM, keyPEM)

	key, _ := pki.GenerateKey()
	csr, _ := pki.CreateNodeCSR(key, "attic", "")
	der, _ := ca.SignNodeCSR(csr, "attic", "", time.Now())

	ag, err := agent.New(agent.Options{NodeID: "attic", Version: "dev", Buffer: agent.NewBuffer(10000, 16<<20), Prober: prober{}, Now: time.Now, Logger: discard})
	if err != nil {
		t.Fatal(err)
	}

	if prefill != nil {
		prefill(ag)
	}

	srv := control.NewNodeServer(control.NodeOptions{
		Agent: ag, HubIdentity: "hub.example.org", Revoked: pki.NewRevokedSet(), HelloTimeout: helloTimeout,
		QueueBytes: queueBytes, Now: time.Now, Logger: discard,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go srv.Run(ctx)

	hs := httptest.NewUnstartedServer(srv)
	hs.TLS = pki.NodeServerConfig(pki.NewCertHolder(tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}), ca.Pool(), pki.NewRevokedSet())
	hs.StartTLS()

	t.Cleanup(func() {
		cancel()
		hs.Close()
	})

	return &node{url: "wss" + strings.TrimPrefix(hs.URL, "https"), ca: ca, agent: ag}
}

func (n *node) dial(t *testing.T, kind, id string) (*wsconn.Conn, error) {
	t.Helper()

	client := pki.NewClientSource(n.ca, kind, id, time.Now)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.HubDialConfig(client, n.ca.Pool(), "attic", nil)}}

	ws, err := wsconn.Dial(context.Background(), n.url, c, rxv1.ControlSubprotocol)
	if err != nil {
		return nil, err
	}

	conn := wsconn.New(context.Background(), ws, wsconn.Options{ReadLimit: control.ReadLimit})
	t.Cleanup(func() { conn.Close(rxv1.CloseNormal, "") })

	return conn, nil
}

func read(t *testing.T, c *wsconn.Conn) rxv1.Envelope {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	env, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v (close %v)", err, wsconn.CloseStatus(err))
	}

	return env
}

func sendEnv(t *testing.T, c *wsconn.Conn, typ rxv1.MessageType, id string, payload any) {
	t.Helper()

	var cid rxv1.CorrelationID
	if id != "" {
		cid = rxv1.MustCorrelationID(id)
	}

	env, err := rxv1.NewEnvelope(typ, cid, time.Now().UnixMilli(), payload)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Send(env); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyTheHubOpensTheControlChannel(t *testing.T) {
	n := startNode(t, time.Second)

	if _, err := n.dial(t, pki.KindGateway, "hub.example.org"); err == nil {
		t.Error("gateway identity accepted on /control")
	}

	if _, err := n.dial(t, pki.KindHub, "other-hub"); err == nil {
		t.Error("unexpected hub identity accepted")
	}
}

func TestHelloTimeout(t *testing.T) {
	n := startNode(t, 100*time.Millisecond)

	c, err := n.dial(t, pki.KindHub, "hub.example.org")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = c.Read(ctx)
	if got := wsconn.CloseStatus(err); got != rxv1.CloseHandshakeTimeout {
		t.Fatalf("close = %v (%v), want 4408", got, err)
	}
}

func TestSessionWelcomeEventsAndAck(t *testing.T) {
	n := startNode(t, time.Second)

	c, err := n.dial(t, pki.KindHub, "hub.example.org")
	if err != nil {
		t.Fatal(err)
	}

	// Before ctl.hello, nothing else is accepted.
	sendEnv(t, c, rxv1.TypeCtlPing, "p", ctl.Empty{})

	if env := read(t, c); env.Type() != rxv1.TypeError {
		t.Fatalf("got %s before hello, want error", env.Type())
	}

	sendEnv(t, c, rxv1.TypeCtlHello, "", ctl.Hello{HubID: "hub.example.org", HubVersion: "dev", Protocols: []string{"rx-ctl.v1"}, ServerTime: time.Now().UnixMilli()})

	welcome := read(t, c)
	if welcome.Type() != rxv1.TypeCtlWelcome {
		t.Fatalf("got %s, want ctl.welcome", welcome.Type())
	}

	var w ctl.Welcome
	if err := welcome.DecodePayload(&w, false); err != nil || w.NodeID != "attic" || w.BootID != n.agent.BootID().String() {
		t.Fatalf("welcome = %+v, %v", w, err)
	}

	caps := read(t, c)

	var seq ctl.SeqOnly
	if caps.Type() != rxv1.TypeNodeCapabilities || caps.DecodePayload(&seq, false) != nil || seq.Seq < 1 {
		t.Fatalf("got %s %s, want node.capabilities", caps.Type(), caps.Payload())
	}

	sendEnv(t, c, rxv1.TypeCtlAck, "", ctl.Ack{UptoSeq: seq.Seq})

	// An unsupported type is answered with unsupported_type; the channel stays up.
	sendEnv(t, c, rxv1.TypeDecodeBatch, "x-1", map[string]any{})

	env := read(t, c)

	var e rxv1.ErrorPayload
	if env.Type() != rxv1.TypeError || env.DecodePayload(&e, false) != nil || e.Code != rxv1.CodeUnsupportedType {
		t.Fatalf("got %s %s, want unsupported_type", env.Type(), env.Payload())
	}

	// ctl.ping is answered.
	sendEnv(t, c, rxv1.TypeCtlPing, "", ctl.Empty{})

	if env := read(t, c); env.Type() != rxv1.TypeCtlPong {
		t.Fatalf("got %s, want ctl.pong", env.Type())
	}

	time.Sleep(50 * time.Millisecond)

	if n.agent.Buffer().Len() != 0 {
		t.Errorf("acked events still buffered: %d", n.agent.Buffer().Len())
	}
}

func TestNewerChannelReplacesOlder(t *testing.T) {
	n := startNode(t, time.Second)

	first, err := n.dial(t, pki.KindHub, "hub.example.org")
	if err != nil {
		t.Fatal(err)
	}

	sendEnv(t, first, rxv1.TypeCtlHello, "", ctl.Hello{HubID: "hub.example.org", ServerTime: time.Now().UnixMilli()})
	_ = read(t, first)

	if _, err := n.dial(t, pki.KindHub, "hub.example.org"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for {
		_, err := first.Read(ctx)
		if err == nil {
			continue
		}

		if got := wsconn.CloseStatus(err); got != rxv1.CloseNormal {
			t.Fatalf("older channel close = %v (%v), want 1000", got, err)
		}

		return
	}
}

// A backlog much larger than the outbound queue is replayed page by page
// instead of overflowing the queue (4413).
func TestReplayLargeBacklog(t *testing.T) {
	const events = 500

	n := startNodeWith(t, time.Second, 8<<10, func(a *agent.Agent) {
		for range events {
			a.Emit(rxv1.TypeConnectionOpened, "", agent.ClassState, func(seq int64) any {
				return ctl.Connection{Seq: seq, CID: strings.Repeat("x", 200)}
			})
		}
	})

	c, err := n.dial(t, pki.KindHub, "hub.example.org")
	if err != nil {
		t.Fatal(err)
	}

	sendEnv(t, c, rxv1.TypeCtlHello, "", ctl.Hello{HubID: "hub.example.org", ServerTime: time.Now().UnixMilli()})

	seen := 0
	for seen < events {
		env := read(t, c)
		if env.Type() != rxv1.TypeConnectionOpened {
			continue
		}

		var s ctl.SeqOnly
		_ = env.DecodePayload(&s, false)
		seen++

		if seen%50 == 0 {
			sendEnv(t, c, rxv1.TypeCtlAck, "", ctl.Ack{UptoSeq: s.Seq})
		}
	}
}
