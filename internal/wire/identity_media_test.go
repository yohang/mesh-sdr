package wire

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

const mediaPassword = "correct horse battery staple"

// hubBrowser is a signed-in browser of the gateway hub.
type hubBrowser struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

func (e *gridEnv) signIn(t *testing.T, user string) *hubBrowser {
	t.Helper()

	jar, _ := cookiejar.New(nil)
	b := &hubBrowser{t: t, base: "http://" + e.gatewayAddr, c: &http.Client{Jar: jar}}
	b.refreshCSRF()

	if st, body, _ := b.form("/login", url.Values{"login": {user}, "password": {mediaPassword}}); st != http.StatusNoContent {
		t.Fatalf("login: %d %s", st, body)
	}

	b.refreshCSRF()

	return b
}

func (b *hubBrowser) refreshCSRF() {
	_, body := b.do(http.MethodGet, "/api/v1/auth/session", "")

	var s struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		b.t.Fatal(err)
	}

	b.csrf = s.Token
}

func (b *hubBrowser) do(method, path, body string) (int, []byte) {
	b.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), method, b.base+path, strings.NewReader(body))
	if err != nil {
		b.t.Fatal(err)
	}

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", b.csrf)
		req.Header.Set("Origin", b.base)
	}

	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	out, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, out
}

// dial opens /nodes/attic/ws through the gateway with the browser's cookies.
func (b *hubBrowser) dial(t *testing.T) *websocket.Conn {
	t.Helper()

	u, _ := url.Parse(b.base)
	h := http.Header{}
	h.Set("Origin", b.base)

	for _, c := range b.c.Jar.Cookies(u) {
		h.Add("Cookie", c.Name+"="+c.Value)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(b.base, "http")+"/nodes/attic/ws", &websocket.DialOptions{
		HTTPHeader: h, Subprotocols: []string{rxv1.Subprotocol},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() { _ = ws.CloseNow() })

	return ws
}

func wsSend(t *testing.T, ws *websocket.Conn, typ rxv1.MessageType, id string, payload any) {
	t.Helper()

	cid := rxv1.CorrelationID{}
	if id != "" {
		cid = rxv1.MustCorrelationID(id)
	}

	env, _ := rxv1.NewEnvelope(typ, cid, time.Now().UnixMilli(), payload)
	b, _ := env.MarshalJSON()

	if err := ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func expectEnvelope(t *testing.T, ws *websocket.Conn, want rxv1.MessageType) rxv1.Envelope {
	t.Helper()

	env, err := readEnvelope(t, ws)
	if err != nil || env.Type() != want {
		t.Fatalf("got %v %s (%v), want %s", env.Type(), env.Payload(), err, want)
	}

	return env
}

func expectWSClose(t *testing.T, ws *websocket.Conn, want rxv1.CloseCode) {
	t.Helper()

	for {
		if _, err := readEnvelope(t, ws); err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusCode(want) {
				t.Fatalf("close status = %d (%v), want %d", got, err, want)
			}

			return
		}
	}
}

func newMediaUser(t *testing.T, e *gridEnv) {
	t.Helper()

	if _, err := UserAdmin(e.hubCfg, quiet, e.adapter).Add(context.Background(), identityapp.AddUserInput{
		Username: "lis", Role: identitydomain.RoleListener, Password: mediaPassword,
	}); err != nil {
		t.Fatal(err)
	}
}

// anonymous is a visitor of the gateway hub without an account.
func (e *gridEnv) anonymous(t *testing.T) *hubBrowser {
	t.Helper()

	jar, _ := cookiejar.New(nil)
	b := &hubBrowser{t: t, base: "http://" + e.gatewayAddr, c: &http.Client{Jar: jar}}
	b.refreshCSRF()

	return b
}

// openMedia opens a media connection as b through the gateway authz and
// returns it with its cid.
func openMedia(t *testing.T, b *hubBrowser) (*websocket.Conn, string) {
	t.Helper()

	ws := b.dial(t)
	wsSend(t, ws, rxv1.TypeSessionHello, "", map[string]any{"client": map[string]string{"name": "test", "version": "1"}})

	var welcome struct {
		CID string `json:"cid"`
	}
	_ = json.Unmarshal(expectEnvelope(t, ws, rxv1.TypeSessionWelcome).Payload(), &welcome)

	return ws, welcome.CID
}

// mint asks POST /api/v1/auth/token for a token of cid on attic.
func (b *hubBrowser) mint(cid string) (int, []byte) {
	return b.do(http.MethodPost, "/api/v1/auth/token", `{"node_id":"attic","cid":"`+cid+`"}`)
}

// openRefreshed opens a media connection as b, then replaces its token with
// one minted by the identity issuer (POST /api/v1/auth/token), bound to the
// connection by the gateway authz.
func openRefreshed(t *testing.T, b *hubBrowser) *websocket.Conn {
	t.Helper()

	ws, cid := openMedia(t, b)

	st, body := b.mint(cid)
	if st != http.StatusOK {
		t.Fatalf("token: %d %s", st, body)
	}

	var minted struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &minted)

	wsSend(t, ws, rxv1.TypeAuthRefresh, "r1", map[string]string{"token": minted.Token})
	expectEnvelope(t, ws, rxv1.TypeAck)

	return ws
}

func newMediaEnv(t *testing.T) *gridEnv {
	t.Helper()

	e := newGridEnvWith(t, true, fastTimings())
	e.enrollNode(t, fakeProber{})
	newMediaUser(t, e)

	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(domain.MustNodeID("attic")) })
	eventually(t, "device registry", 5*time.Second, func() bool {
		_, err := e.g.devices.Get(context.Background(), "hf")

		return err == nil
	})

	return e
}

// A refresh computes the scopes again with the listen_policy setting of
// the settings store: once an admin sets it to registered, an anonymous
// connection opened before gets no token.
func TestTokenRefreshFollowsListenPolicy(t *testing.T) {
	e := newMediaEnv(t)

	if _, err := UserAdmin(e.hubCfg, quiet, e.adapter).Add(context.Background(), identityapp.AddUserInput{
		Username: "root", Role: identitydomain.RoleAdmin, Password: mediaPassword,
	}); err != nil {
		t.Fatal(err)
	}

	anon := e.anonymous(t)
	_, cid := openMedia(t, anon)

	if st, body := anon.mint(cid); st != http.StatusOK {
		t.Fatalf("anonymous token under the anonymous policy: %d %s", st, body)
	}

	root := e.signIn(t, "root")
	if st, body, _ := root.form("/admin/access", url.Values{"section": {"listening"}, "listen_policy": {"registered"}}); st != http.StatusOK {
		t.Fatalf("set listen_policy: %d %s", st, body)
	}

	if st, body := anon.mint(cid); st != http.StatusForbidden || !strings.Contains(string(body), "no_listenable_device") {
		t.Fatalf("anonymous token under the registered policy: %d %s, want 403 no_listenable_device", st, body)
	}

	_ = openRefreshed(t, e.signIn(t, "lis")).CloseNow()
}

// POST /api/v1/auth/token mints only for an open connection the gateway
// authz issued on that node to the same caller.
func TestTokenConnectionBinding(t *testing.T) {
	e := newMediaEnv(t)
	ctx := context.Background()
	repo := gridsqlite.NewConnectionRepository(e.adapter)

	first, second, anon := e.signIn(t, "lis"), e.signIn(t, "lis"), e.anonymous(t)
	_, userCID := openMedia(t, first)
	_, anonCID := openMedia(t, anon)

	// row stores a presence row the way another path would have left it.
	row := func(info domain.ConnectionInfo, closed bool) string {
		t.Helper()

		id, _ := shared.NewUUIDv7(time.Now())
		info.ID, info.Kind, info.NodeID, info.IP = id, domain.ConnectionMedia, "attic", "192.0.2.1"

		c, err := domain.NewConnection(info, time.Now())
		if err != nil {
			t.Fatal(err)
		}

		if closed {
			c.Close(domain.CloseNodeLost, time.Now())
		}

		if _, err := repo.Open(ctx, c); err != nil {
			t.Fatal(err)
		}

		return id.String()
	}

	userRole := int(identitydomain.RoleListener.ID())

	refused := []struct {
		name string
		b    *hubBrowser
		node string
		cid  string
	}{
		{"another session of the same user", second, "attic", userCID},
		{"anonymous caller on a user's connection", anon, "attic", userCID},
		{"user on an anonymous connection", first, "attic", anonCID},
		{"another node", anon, "cellar", anonCID},
		{"a cid the gateway never issued", anon, "attic", shared.UUID{}.String()},
		{"a closed anonymous connection", anon, "attic", row(domain.ConnectionInfo{HubIssued: true}, true)},
		{"an anonymous row reported by a node", anon, "attic", row(domain.ConnectionInfo{}, false)},
		{"a row anonymised by an account erasure", anon, "attic", row(domain.ConnectionInfo{HubIssued: true, RoleID: userRole}, false)},
	}

	for _, tc := range refused {
		st, body := tc.b.do(http.MethodPost, "/api/v1/auth/token", `{"node_id":"`+tc.node+`","cid":"`+tc.cid+`"}`)
		if st != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", tc.name, st, body)
		}
	}

	// The holders themselves get their tokens.
	if st, body := first.mint(userCID); st != http.StatusOK {
		t.Errorf("user's own connection: %d %s", st, body)
	}

	if st, body := anon.mint(anonCID); st != http.StatusOK {
		t.Errorf("anonymous visitor's own connection: %d %s", st, body)
	}
}

// TestIdentityTokensOnNodes wires identity (ACC-007) into the grid: a token
// minted by the identity issuer refreshes a gateway connection; a session
// revoked through identity closes it on the node with 4403; a signing key
// revoked in the keyring closes the connections it signed.
func TestIdentityTokensOnNodes(t *testing.T) {
	e := newMediaEnv(t)

	// Anonymous visitors refresh their tokens too: the gateway authz bound
	// the connection to them.
	_ = openRefreshed(t, e.anonymous(t)).CloseNow()

	first := e.signIn(t, "lis")
	ws := openRefreshed(t, first)

	// The user signs the first session out from a second one.
	second := e.signIn(t, "lis")

	st, body := first.do(http.MethodGet, "/account", "")
	current := regexp.MustCompile(`(?s)\(this session\).*?(/account/sessions/[A-Za-z0-9_-]+/revoke)`).FindSubmatch(body)

	if st != http.StatusOK || current == nil {
		t.Fatalf("sessions: %d %s", st, body)
	}

	if st, body, _ := second.form(string(current[1]), nil); st != http.StatusOK {
		t.Fatalf("revoke session: %d %s", st, body)
	}

	expectWSClose(t, ws, rxv1.CloseForbidden)

	// A key revoked in the keyring closes the connections it signed.
	ws = openRefreshed(t, second)

	keys, _ := e.g.keys.VerificationKeys(context.Background())
	if len(keys.Keys) == 0 {
		t.Fatal("no published key")
	}

	k, _ := e.g.keys.keyring()
	for _, jwk := range keys.Keys {
		if err := k.Revoke(jwk.Kid, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	expectWSClose(t, ws, rxv1.CloseUnauthenticated)
}

// The sid claim and ctl.revocations name sessions with one function.
func TestSessionRefIsTheTokenSessionRef(t *testing.T) {
	id, _ := shared.NewUUIDv7(time.Now())
	sid, _ := identitydomain.NewSessionID(id)
	uid, _ := identitydomain.NewUserID(id)

	s, err := identitydomain.RehydrateSession(identitydomain.SessionState{ID: sid, UserID: uid, Provider: identitydomain.ProviderLocal})
	if err != nil {
		t.Fatal(err)
	}

	if s.Ref() != token.SessionRef(sid.String()) {
		t.Fatalf("Session.Ref() = %s, token.SessionRef = %s", s.Ref(), token.SessionRef(sid.String()))
	}
}
