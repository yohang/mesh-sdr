package media_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/grid/infra/agent"
	"github.com/yohang/mesh-sdr/internal/grid/infra/media"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	mediapkg "github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

const (
	issuer = "https://sdr.example.org"
	origin = "https://sdr.example.org"
	hubID  = "sdr.example.org"
)

type env struct {
	srv    *media.Server
	url    string
	ca     *pki.CA
	key    ed25519.PrivateKey
	client *http.Client
}

func newEnv(t *testing.T, opts ...func(*media.Options)) *env {
	t.Helper()

	certPEM, keyPEM, err := pki.GenerateCA("test", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	ca, err := pki.ParseCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	nodeKey, _ := pki.GenerateKey()

	csr, err := pki.CreateNodeCSR(nodeKey, "roof", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	der, err := ca.SignNodeCSR(csr, "roof", "127.0.0.1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	serverCert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: nodeKey}

	o := media.Options{
		NodeID: "roof", Version: "1.0.0", GatewayIdentity: hubID, OwnSerial: func() string { return "AB" },
		Now: time.Now, Logger: slog.New(slog.DiscardHandler),
	}
	for _, f := range opts {
		f(&o)
	}

	srv := media.NewServer(o)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { srv.Run(ctx); close(done) }()

	ts := httptest.NewUnstartedServer(srv)
	ts.TLS = pki.NodeServerConfig(pki.NewCertHolder(serverCert), ca.Pool(), nil)
	ts.StartTLS()

	t.Cleanup(func() {
		cancel()
		<-done
		ts.Close()
	})

	_, key, _ := ed25519.GenerateKey(rand.Reader)

	return &env{srv: srv, url: "wss" + strings.TrimPrefix(ts.URL, "https"), ca: ca, key: key, client: clientAs(t, ca, pki.KindGateway, hubID)}
}

func clientAs(t *testing.T, ca *pki.CA, kind, id string) *http.Client {
	t.Helper()

	cert, err := ca.MintClient(kind, id, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: ca.Pool(), ServerName: pki.NodeServerName("roof"),
		Certificates: []tls.Certificate{cert},
	}}}
}

func (e *env) installKeys(t *testing.T, keys ...ed25519.PrivateKey) {
	t.Helper()

	u := ctl.KeysUpdate{Issuer: issuer}
	for _, k := range keys {
		u.Keys = append(u.Keys, token.NewJWK(k.Public().(ed25519.PublicKey)))
	}

	if err := e.srv.UpdateKeys(u); err != nil {
		t.Fatal(err)
	}
}

func (e *env) token(t *testing.T, cid string, exp time.Time, mut ...func(*token.Claims)) string {
	t.Helper()

	c := token.Claims{
		Issuer: issuer, Audience: token.Audience("roof"), Subject: "u1", SessionID: token.SessionRef("s1"), ConnectionID: cid,
		Roles:    []string{"listener"},
		Scopes:   []token.Scope{{Device: "hf", Perms: []string{token.PermListen, token.PermDemod}}},
		IssuedAt: time.Now().Add(-time.Second), NotBefore: time.Now().Add(-time.Second), ExpiresAt: exp,
	}
	for _, m := range mut {
		m(&c)
	}

	raw, err := token.Sign(c, e.key)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// dial opens /ws; it returns the connection or the HTTP status of a refusal.
func (e *env) dial(t *testing.T, client *http.Client, tok, cid, org string) (*websocket.Conn, int) {
	t.Helper()

	h := http.Header{}
	h.Set("Origin", org)
	h.Set(media.HeaderAccessToken, tok)
	h.Set(media.HeaderCID, cid)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := websocket.Dial(ctx, e.url+"/ws", &websocket.DialOptions{
		HTTPClient: client, HTTPHeader: h, Subprotocols: []string{rxv1.Subprotocol},
	})
	if resp != nil && resp.Body != nil {
		if err != nil {
			b, _ := io.ReadAll(resp.Body)
			t.Logf("refused: %d %s", resp.StatusCode, b)
		}

		_ = resp.Body.Close()
	}

	if err != nil {
		if resp == nil {
			t.Fatalf("dial: %v", err)
		}

		return nil, resp.StatusCode
	}

	t.Cleanup(func() { _ = ws.CloseNow() })

	return ws, http.StatusSwitchingProtocols
}

func send(t *testing.T, ws *websocket.Conn, typ rxv1.MessageType, id string, payload any) {
	t.Helper()

	cid := rxv1.CorrelationID{}
	if id != "" {
		cid = rxv1.MustCorrelationID(id)
	}

	env, err := rxv1.NewEnvelope(typ, cid, time.Now().UnixMilli(), payload)
	if err != nil {
		t.Fatal(err)
	}

	b, _ := env.MarshalJSON()

	if err := ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, ws *websocket.Conn) (rxv1.Envelope, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, b, err := ws.Read(ctx)
	if err != nil {
		return rxv1.Envelope{}, err
	}

	return rxv1.DecodeEnvelope(b)
}

func expectType(t *testing.T, ws *websocket.Conn, want rxv1.MessageType) rxv1.Envelope {
	t.Helper()

	env, err := read(t, ws)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if env.Type() != want {
		t.Fatalf("got %s %s, want %s", env.Type(), env.Payload(), want)
	}

	return env
}

func expectClose(t *testing.T, ws *websocket.Conn, want websocket.StatusCode) {
	t.Helper()

	for {
		_, err := read(t, ws)
		if err == nil {
			continue
		}

		if got := websocket.CloseStatus(err); got != want {
			t.Fatalf("close status = %d (%v), want %d", got, err, want)
		}

		return
	}
}

func hello(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()

	send(t, ws, rxv1.TypeSessionHello, "", map[string]any{"client": map[string]string{"name": "test", "version": "1"}})

	var w map[string]any
	if err := json.Unmarshal(expectType(t, ws, rxv1.TypeSessionWelcome).Payload(), &w); err != nil {
		t.Fatal(err)
	}

	return w
}

func TestRefusals(t *testing.T) {
	e := newEnv(t)
	exp := time.Now().Add(5 * time.Minute)

	if _, st := e.dial(t, e.client, e.token(t, "c1", exp), "c1", origin); st != http.StatusServiceUnavailable {
		t.Fatalf("no keys: status %d, want 503", st)
	}

	e.installKeys(t, e.key)

	_, other, _ := ed25519.GenerateKey(rand.Reader)

	cases := []struct {
		name   string
		client *http.Client
		tok    string
		cid    string
		origin string
		want   int
	}{
		{"hub certificate", clientAs(t, e.ca, pki.KindHub, hubID), e.token(t, "c1", exp), "c1", origin, http.StatusForbidden},
		{"other gateway", clientAs(t, e.ca, pki.KindGateway, "evil.example.org"), e.token(t, "c1", exp), "c1", origin, http.StatusForbidden},
		{"origin", e.client, e.token(t, "c1", exp), "c1", "https://evil.example.org", http.StatusForbidden},
		{"no token", e.client, "", "c1", origin, http.StatusUnauthorized},
		{"expired", e.client, e.token(t, "c1", time.Now().Add(-time.Minute)), "c1", origin, http.StatusUnauthorized},
		{"other node", e.client, e.token(t, "c1", exp, func(c *token.Claims) { c.Audience = token.Audience("shack") }), "c1", origin, http.StatusUnauthorized},
		{"unknown key", e.client, func() string {
			raw, _ := token.Sign(token.Claims{Issuer: issuer, Audience: token.Audience("roof"), ConnectionID: "c1", ExpiresAt: exp}, other)

			return raw
		}(), "c1", origin, http.StatusUnauthorized},
		{"cid mismatch", e.client, e.token(t, "c1", exp), "c2", origin, http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, st := e.dial(t, tc.client, tc.tok, tc.cid, tc.origin); st != tc.want {
				t.Fatalf("status %d, want %d", st, tc.want)
			}
		})
	}
}

func TestSession(t *testing.T) {
	e := newEnv(t)
	e.installKeys(t, e.key)

	exp := time.Now().Add(5 * time.Minute)

	ws, st := e.dial(t, e.client, e.token(t, "c1", exp), "c1", origin)
	if st != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", st)
	}

	if _, st := e.dial(t, e.client, e.token(t, "c1", exp), "c1", origin); st != http.StatusConflict {
		t.Fatalf("second use of a cid: status %d, want 409", st)
	}

	// Nothing but session.hello before the welcome.
	send(t, ws, rxv1.TypeDeviceAttach, "a0", map[string]string{"device_id": "hf"})
	expectType(t, ws, rxv1.TypeError)

	w := hello(t, ws)
	if w["cid"] != "c1" || w["user"].(map[string]any)["id"] != "u1" {
		t.Fatalf("welcome = %v", w)
	}

	// Scope checks of device-scoped messages.
	send(t, ws, rxv1.TypeDeviceAttach, "a1", map[string]string{"device_id": "vhf"})

	if p := string(expectType(t, ws, rxv1.TypeError).Payload()); !strings.Contains(p, `"forbidden"`) {
		t.Fatalf("attach out of scope: %s", p)
	}

	send(t, ws, rxv1.TypeDeviceRetune, "a2", map[string]string{"device_id": "hf"})

	if p := string(expectType(t, ws, rxv1.TypeError).Payload()); !strings.Contains(p, `"forbidden"`) {
		t.Fatalf("retune without the permission: %s", p)
	}

	// Refresh: another cid is refused, the same one is acknowledged.
	send(t, ws, rxv1.TypeAuthRefresh, "r1", map[string]string{"token": e.token(t, "c9", exp)})

	if p := string(expectType(t, ws, rxv1.TypeError).Payload()); !strings.Contains(p, "token_invalid") {
		t.Fatalf("refresh of another cid: %s", p)
	}

	send(t, ws, rxv1.TypeAuthRefresh, "r2", map[string]string{"token": e.token(t, "c1", exp.Add(time.Minute))})
	expectType(t, ws, rxv1.TypeAck)

	// A revoked session is closed with 4403.
	e.srv.Revoke(ctl.Revocations{Sessions: []ctl.Revoked{{ID: token.SessionRef("s1"), At: time.Now().UnixMilli()}}})
	expectClose(t, ws, websocket.StatusCode(rxv1.CloseForbidden))

	// Its tokens are refused afterwards.
	if _, st := e.dial(t, e.client, e.token(t, "c2", exp), "c2", origin); st != http.StatusForbidden {
		t.Fatalf("token of a revoked session: status %d, want 403", st)
	}
}

func TestExpiry(t *testing.T) {
	e := newEnv(t)
	e.installKeys(t, e.key)

	// Expired 28.5 s ago (exp has a 1 s resolution): accepted within the
	// leeway, closed about 1.5 s later.
	ws, st := e.dial(t, e.client, e.token(t, "c1", time.Now().Add(-token.Leeway+1500*time.Millisecond)), "c1", origin)
	if st != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", st)
	}

	hello(t, ws)
	expectClose(t, ws, websocket.StatusCode(rxv1.CloseUnauthenticated))
}

func TestWithdrawn(t *testing.T) {
	e := newEnv(t)
	e.installKeys(t, e.key)

	exp := time.Now().Add(5 * time.Minute)

	ws, _ := e.dial(t, e.client, e.token(t, "c1", exp), "c1", origin)
	hello(t, ws)

	// The node's own certificate serial is revoked.
	e.srv.Revoke(ctl.Revocations{CertSerials: []string{"AB"}})
	expectClose(t, ws, websocket.StatusCode(rxv1.CloseForbidden))

	if _, st := e.dial(t, e.client, e.token(t, "c2", exp), "c2", origin); st != http.StatusServiceUnavailable {
		t.Fatalf("withdrawn node: status %d, want 503", st)
	}

	// A new control channel installs keys again.
	e.installKeys(t, e.key)

	ws, _ = e.dial(t, e.client, e.token(t, "c3", exp), "c3", origin)
	hello(t, ws)

	// Dropping the signing key closes its sessions.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	e.installKeys(t, other)
	expectClose(t, ws, websocket.StatusCode(rxv1.CloseUnauthenticated))

	if e.srv.Count() != 0 {
		// The registry is cleaned when the handler returns.
		deadline := time.Now().Add(2 * time.Second)
		for e.srv.Count() != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}

	if n := e.srv.Count(); n != 0 {
		t.Fatalf("open sessions = %d", n)
	}

}

// TestDemodScopeAndStrikes: demod.create needs the demod permission on the
// device, and more than ten forbidden messages in a minute close the
// connection with 4403 (§5.9, AUTH-004).
func TestDemodScopeAndStrikes(t *testing.T) {
	e := newEnv(t)
	e.installKeys(t, e.key)

	exp := time.Now().Add(5 * time.Minute)

	ws, st := e.dial(t, e.client, e.token(t, "c1", exp, func(c *token.Claims) {
		c.Scopes = []token.Scope{{Device: "hf", Perms: []string{token.PermListen}}}
	}), "c1", origin)
	if st != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", st)
	}

	hello(t, ws)

	send(t, ws, rxv1.TypeDemodCreate, "d0", map[string]any{"device_id": "hf", "mode": "usb", "offset_hz": 0})

	if p := string(expectType(t, ws, rxv1.TypeError).Payload()); !strings.Contains(p, `"forbidden"`) {
		t.Fatalf("demod.create without the demod permission: %s", p)
	}

	// Allowed: not implemented yet, but not forbidden.
	send(t, ws, rxv1.TypeDeviceAttach, "a0", map[string]any{"device_id": "hf"})

	if p := string(expectType(t, ws, rxv1.TypeError).Payload()); !strings.Contains(p, "unsupported_type") {
		t.Fatalf("attach in scope: %s", p)
	}

	// Nine more refusals (ten in the minute) keep the connection open.
	for i := range media.MaxForbiddenPerMinute - 1 {
		send(t, ws, rxv1.TypeDeviceAttach, "f"+strconv.Itoa(i), map[string]any{"device_id": "vhf"})
		expectType(t, ws, rxv1.TypeError)
	}

	// The eleventh closes it.
	send(t, ws, rxv1.TypeDeviceAttach, "f-last", map[string]any{"device_id": "vhf"})
	expectClose(t, ws, websocket.StatusCode(rxv1.CloseForbidden))
}

// TestListenPolicyOnNode: the node re-checks the effective listen policy
// (SRC-023) and the token scope: an anonymous token never acts on a
// registered device, even when its scope names it, and never holds the
// preset or retune permissions.
func TestListenPolicyOnNode(t *testing.T) {
	// The real desired state: node config overrides, global policy from
	// the hub.
	state := agent.NewDesiredState([]ctl.Device{{ID: "hf", ListenPolicy: "anonymous"}, {ID: "vhf", ListenPolicy: "registered"}})
	state.Apply(ctl.StateApply{Revision: 1, Policy: ctl.StatePolicy{ListenPolicy: "anonymous"}})

	e := newEnv(t, func(o *media.Options) { o.Policy = state })
	e.installKeys(t, e.key)

	exp := time.Now().Add(5 * time.Minute)
	all := []string{token.PermListen, token.PermDemod, token.PermPreset, token.PermRetune}

	cases := []struct {
		name      string
		anonymous bool
		device    string
		typ       rxv1.MessageType
		forbidden bool
	}{
		{"anonymous attach on an anonymous device", true, "hf", rxv1.TypeDeviceAttach, false},
		{"anonymous attach on a registered device", true, "vhf", rxv1.TypeDeviceAttach, true},
		{"anonymous demod on a registered device", true, "vhf", rxv1.TypeDemodCreate, true},
		{"anonymous preset select", true, "hf", rxv1.TypePresetSelect, true},
		{"anonymous retune", true, "hf", rxv1.TypeDeviceRetune, true},
		{"user attach on a registered device", false, "vhf", rxv1.TypeDeviceAttach, false},
		{"user preset select on a registered device", false, "vhf", rxv1.TypePresetSelect, false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cid := "p" + strconv.Itoa(i)
			tok := e.token(t, cid, exp, func(c *token.Claims) {
				if tc.anonymous {
					// A token scoped by mistake: listen and demod on both
					// devices, as the hub would never issue it.
					c.Subject, c.SessionID, c.Roles = token.AnonymousSubject, "", nil
					c.Scopes = []token.Scope{
						{Device: "hf", Perms: []string{token.PermListen, token.PermDemod}},
						{Device: "vhf", Perms: []string{token.PermListen, token.PermDemod}},
					}

					return
				}

				c.Scopes = []token.Scope{{Device: "hf", Perms: all}, {Device: "vhf", Perms: all}}
			})

			ws, st := e.dial(t, e.client, tok, cid, origin)
			if st != http.StatusSwitchingProtocols {
				t.Fatalf("status %d", st)
			}

			hello(t, ws)

			payload := map[string]any{"device_id": tc.device}
			switch tc.typ {
			case rxv1.TypeDemodCreate:
				payload["mode"], payload["offset_hz"] = "usb", 0
			case rxv1.TypePresetSelect:
				payload["preset_id"] = "p"
			case rxv1.TypeDeviceRetune:
				payload["center_freq"] = 14_000_000
			}

			send(t, ws, tc.typ, "m0", payload)

			p := string(expectType(t, ws, rxv1.TypeError).Payload())
			if got := strings.Contains(p, `"forbidden"`); got != tc.forbidden {
				t.Fatalf("forbidden = %v, want %v: %s", got, tc.forbidden, p)
			}
		})
	}
}

// anonToken makes the token of an anonymous visitor scoped to listen on
// hf.
func anonToken(c *token.Claims) {
	c.Subject, c.SessionID, c.Roles = token.AnonymousSubject, "", nil
	c.Scopes = []token.Scope{{Device: "hf", Perms: []string{token.PermListen, token.PermDemod}}}
}

// spyStreams records what the stream handler sees of the connection.
type spyStreams struct {
	handled      chan rxv1.MessageType
	reauthorized chan token.Claims
}

func (s *spyStreams) Open(p mediapkg.Peer) mediapkg.StreamSession { return &spySession{s: s, p: p} }

type spySession struct {
	s *spyStreams
	p mediapkg.Peer
}

func (ss *spySession) Handle(_ context.Context, req rxv1.Envelope) {
	ss.s.handled <- req.Type()
	ss.p.Ack(req, struct{}{})
}

func (ss *spySession) Reauthorize() { ss.s.reauthorized <- ss.p.Claims() }

func (ss *spySession) Close() {}

// TestListenPolicyChangeMidSession: a device that becomes registered while
// an anonymous visitor listens is dropped at the next auth.refresh (the
// stream handler's Reauthorize sees claims without it), and new messages
// for it are refused at once.
func TestListenPolicyChangeMidSession(t *testing.T) {
	state := agent.NewDesiredState([]ctl.Device{{ID: "hf"}})
	state.Apply(ctl.StateApply{Revision: 1, Policy: ctl.StatePolicy{ListenPolicy: "anonymous"}})

	spy := &spyStreams{handled: make(chan rxv1.MessageType, 4), reauthorized: make(chan token.Claims, 4)}
	e := newEnv(t, func(o *media.Options) { o.Policy, o.Streams = state, spy })
	e.installKeys(t, e.key)

	exp := time.Now().Add(5 * time.Minute)

	ws, st := e.dial(t, e.client, e.token(t, "m1", exp, anonToken), "m1", origin)
	if st != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", st)
	}

	hello(t, ws)

	send(t, ws, rxv1.TypeDeviceAttach, "a0", map[string]any{"device_id": "hf"})
	expectType(t, ws, rxv1.TypeAck)

	if got := <-spy.handled; got != rxv1.TypeDeviceAttach {
		t.Fatalf("handled %s", got)
	}

	// The hub switches the global policy to registered.
	state.Apply(ctl.StateApply{Revision: 2, Policy: ctl.StatePolicy{ListenPolicy: "registered"}})

	send(t, ws, rxv1.TypeAuthRefresh, "r1", map[string]string{"token": e.token(t, "m1", exp.Add(time.Minute), anonToken)})
	expectType(t, ws, rxv1.TypeAck)

	if c := <-spy.reauthorized; c.Allows("hf", token.PermListen) || len(c.Scopes) != 0 {
		t.Fatalf("reauthorized with %+v", c.Scopes)
	}

	send(t, ws, rxv1.TypeDeviceAttach, "a1", map[string]any{"device_id": "hf"})

	if p := string(expectType(t, ws, rxv1.TypeError).Payload()); !strings.Contains(p, `"forbidden"`) {
		t.Fatalf("attach after the change: %s", p)
	}
}

// TestListenPolicyRefusalsAreStrikes: refused anonymous attaches on a
// registered device count like scope refusals; the eleventh in a minute
// closes the connection with 4403.
func TestListenPolicyRefusalsAreStrikes(t *testing.T) {
	state := agent.NewDesiredState([]ctl.Device{{ID: "hf", ListenPolicy: "registered"}})
	e := newEnv(t, func(o *media.Options) { o.Policy = state })
	e.installKeys(t, e.key)

	ws, st := e.dial(t, e.client, e.token(t, "s1", time.Now().Add(5*time.Minute), anonToken), "s1", origin)
	if st != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", st)
	}

	hello(t, ws)

	for i := range media.MaxForbiddenPerMinute {
		send(t, ws, rxv1.TypeDeviceAttach, "f"+strconv.Itoa(i), map[string]any{"device_id": "hf"})

		if p := string(expectType(t, ws, rxv1.TypeError).Payload()); !strings.Contains(p, "signed-in listener") {
			t.Fatalf("refusal %d: %s", i, p)
		}
	}

	send(t, ws, rxv1.TypeDeviceAttach, "f-last", map[string]any{"device_id": "hf"})
	expectClose(t, ws, websocket.StatusCode(rxv1.CloseForbidden))
}

// TestPresetSelectScope: preset.select needs the preset permission on the
// device; with it, the message reaches the device stream handler.
func TestPresetSelectScope(t *testing.T) {
	e := newEnv(t)
	e.installKeys(t, e.key)

	exp := time.Now().Add(5 * time.Minute)

	for _, tt := range []struct {
		perms []string
		want  string
	}{
		{[]string{token.PermListen, token.PermDemod}, `"forbidden"`},
		{[]string{token.PermListen, token.PermPreset}, "device streaming is not available"},
	} {
		cid := "c-" + strings.Join(tt.perms, "-")

		ws, st := e.dial(t, e.client, e.token(t, cid, exp, func(c *token.Claims) {
			c.Scopes = []token.Scope{{Device: "hf", Perms: tt.perms}}
		}), cid, origin)
		if st != http.StatusSwitchingProtocols {
			t.Fatalf("status %d", st)
		}

		hello(t, ws)
		send(t, ws, rxv1.TypePresetSelect, "p0", map[string]any{"device_id": "hf", "preset_id": "p"})

		if p := string(expectType(t, ws, rxv1.TypeError).Payload()); !strings.Contains(p, tt.want) {
			t.Errorf("preset.select with %v: %s", tt.perms, p)
		}
	}
}
