package gateway_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/grid/infra/gateway"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = ln.Close() }()

	return ln.Addr().String()
}

type fixture struct {
	ca       *pki.CA
	node     *httptest.Server
	evil     *httptest.Server
	gwClient *pki.CertSource
	logs     *syncBuffer
	// check is the extra leaf check of the node dial (pin, revocation).
	check pki.LeafCheck
}

// nodeServer starts a fake node id with a certificate of ca.
func nodeServer(t *testing.T, ca *pki.CA, id string, h http.Handler) *httptest.Server {
	t.Helper()

	key, _ := pki.GenerateKey()
	csr, _ := pki.CreateNodeCSR(key, id, "127.0.0.1:0")

	der, err := ca.SignNodeCSR(csr, id, "127.0.0.1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(h)
	srv.TLS = pki.NodeServerConfig(pki.NewCertHolder(tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}), ca.Pool(), nil)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv
}

// newFixture starts a fake node "roof": mTLS with the hub CA, /ws sends
// what it received as its first message, then echoes. Its 101 answer
// carries a cookie and a header of its own, which the browser must not
// see. A node "evil" answers with content of its own.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	certPEM, keyPEM, err := pki.GenerateCA("test", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	ca, _ := pki.ParseCA(certPEM, keyPEM)

	node := nodeServer(t, ca, "roof", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, id, _ := pki.PeerIdentity(r)

		w.Header().Set("Set-Cookie", "__Host-rx_session=stolen; Path=/; Secure")
		w.Header().Set("X-Node-Private", "1")

		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{"rx.v1"}})
		if err != nil {
			return
		}

		defer func() { _ = ws.CloseNow() }()

		var names []string
		for k := range r.Header {
			names = append(names, k)
		}

		slices.Sort(names)

		hello, _ := json.Marshal(map[string]string{
			"path": r.URL.Path, "query": r.URL.RawQuery, "peer": kind + ":" + id, "token": r.Header.Get(gateway.HeaderAccessToken),
			"cid": r.Header.Get(gateway.HeaderCID), "node": r.Header.Get(gateway.HeaderNodeID),
			"forged": r.Header.Get("X-Rx-Forged"), "upstream": r.Header.Get(gateway.HeaderUpstream),
			"cookie": r.Header.Get("Cookie"), "authorization": r.Header.Get("Authorization"),
			"referer": r.Header.Get("Referer"), "origin": r.Header.Get("Origin"), "headers": strings.Join(names, ","),
		})

		ctx := r.Context()
		_ = ws.Write(ctx, websocket.MessageText, hello)

		for {
			typ, b, err := ws.Read(ctx)
			if err != nil {
				return
			}

			_ = ws.Write(ctx, typ, b)
		}
	}))

	evil := nodeServer(t, ca, "evil", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Host-rx_session", Value: "stolen", Path: "/", Secure: true})
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<script>steal()</script>")
	}))

	return &fixture{ca: ca, node: node, evil: evil, gwClient: pki.NewClientSource(ca, pki.KindGateway, "hub.example.org", time.Now), logs: &syncBuffer{}}
}

func hostOf(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "https://") }

// hub is a fake hub router: "/" answers "hub", the authz admits node "roof"
// with the cookie session=ok and refuses the others like grid/http's
// AuthzHandler would.
func (f *fixture) hub() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hub") })
	mux.HandleFunc(gateway.AuthzPath, func(w http.ResponseWriter, r *http.Request) {
		refuse := func(status int, code string) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.Header().Set(gateway.HeaderUpstream, "leak:1")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"code":"`+code+`"}`)
		}

		grant := func(upstream string) {
			w.Header().Set(gateway.HeaderAccessToken, "minted")
			w.Header().Set(gateway.HeaderCID, "c1")
			w.Header().Set(gateway.HeaderUpstream, upstream)
			w.WriteHeader(http.StatusNoContent)
		}

		switch node := r.URL.Query().Get("node"); {
		case r.Header.Get(gateway.HeaderAccessToken) != "" || r.Header.Get("X-Rx-Forged") != "":
			// Forged headers must never reach the authz (the dial below
			// sends them).
			w.WriteHeader(http.StatusTeapot)
		case r.Method != http.MethodGet || r.Header.Get("Upgrade") != "":
			w.WriteHeader(http.StatusExpectationFailed)
		case node == "offline":
			refuse(http.StatusServiceUnavailable, "node_offline")
		case node == "foreign":
			refuse(http.StatusForbidden, "origin_denied")
		case node == "busy":
			w.Header().Set("Retry-After", "7")
			refuse(http.StatusTooManyRequests, "rate_limited")
		case node == "evil":
			grant(hostOf(f.evil))
		case node == "gone":
			grant(freeAddrNoListen)
		case node == "nowhere":
			w.WriteHeader(http.StatusNoContent)
		case node == "liar":
			// A node id the certificate of roof does not carry.
			grant(hostOf(f.node))
		case node != "roof":
			refuse(http.StatusNotFound, "node_not_found")
		default:
			if c, err := r.Cookie("session"); err != nil || c.Value != "ok" {
				refuse(http.StatusUnauthorized, "unauthenticated")

				return
			}

			grant(hostOf(f.node))
		}
	})

	return mux
}

// freeAddrNoListen is a loopback address nothing listens on.
const freeAddrNoListen = "127.0.0.1:1"

func (f *fixture) options(c gateway.Config) gateway.Options {
	return gateway.Options{
		Config: c, Hub: f.hub(),
		NodeTLS: func(_ context.Context, id string) (*tls.Config, error) {
			return pki.HubDialConfig(f.gwClient, f.ca.Pool(), id, f.check), nil
		},
		Logger: slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func (f *fixture) start(t *testing.T, c gateway.Config) *gateway.Gateway {
	t.Helper()

	return startGateway(t, f.options(c))
}

func startGateway(t *testing.T, o gateway.Options) *gateway.Gateway {
	t.Helper()

	g, err := gateway.New(o)
	if err != nil {
		t.Fatal(err)
	}

	if err := g.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := g.Stop(); err != nil {
			t.Error(err)
		}
	})

	return g
}

func dial(t *testing.T, client *http.Client, url string, h http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: client, HTTPHeader: h, Subprotocols: []string{"rx.v1"}})
	if err == nil {
		t.Cleanup(func() { _ = ws.CloseNow() })
	}

	return ws, resp, err
}

func httpGet(t *testing.T, url string) (*http.Response, string) {
	t.Helper()

	resp, err := http.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	return resp, string(body)
}

func TestGatewayPlainHTTP(t *testing.T) {
	f := newFixture(t)
	addr := freeAddr(t)
	f.start(t, gateway.Config{TLSMode: gateway.TLSOff, HTTPListen: addr, PublicURL: "http://" + addr})

	base := "http://" + addr

	resp, body := httpGet(t, base+"/")
	if resp.StatusCode != 200 || body != "hub" || resp.Header.Get("Server") != "" {
		t.Fatalf("hub: %d %q server=%q", resp.StatusCode, body, resp.Header.Get("Server"))
	}

	// The authz route and every other path under /nodes/ and /internal/
	// answer 404 from outside, whatever their case and dot segments.
	for _, path := range []string{
		gateway.AuthzPath + "?node=roof", "/INTERNAL/gateway/authz?node=roof", "/static/../internal/gateway/authz?node=roof",
		"/nodes/x/ws", "/nodes/roof/other", "/nodes/ROOF/ws", "/nodes/",
	} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/", nil)
		req.URL.Opaque = path // sent as is, not cleaned by the client

		if strings.Contains(path, "?") {
			req.URL.Opaque, req.URL.RawQuery, _ = strings.Cut(path, "?")
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Server") != "" {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}

	ws := "ws://" + addr

	// Authz answers pass through with their body and headers (but no
	// X-Rx-*); no upgrade happens.
	cases := []struct {
		path   string
		cookie string
		want   int
		code   string
	}{
		{"/nodes/shack/ws", "ok", http.StatusNotFound, "node_not_found"},
		{"/nodes/offline/ws", "ok", http.StatusServiceUnavailable, "node_offline"},
		{"/nodes/roof/ws", "", http.StatusUnauthorized, "unauthenticated"},
		{"/nodes/foreign/ws", "ok", http.StatusForbidden, "origin_denied"},
		{"/nodes/busy/ws", "ok", http.StatusTooManyRequests, "rate_limited"},
		{"/nodes/nowhere/ws", "ok", http.StatusBadGateway, ""},
	}

	for _, tc := range cases {
		h := http.Header{}
		if tc.cookie != "" {
			h.Set("Cookie", "session="+tc.cookie)
		}

		_, resp, err := dial(t, nil, ws+tc.path, h)
		if err == nil || resp == nil || resp.StatusCode != tc.want {
			t.Fatalf("%s: err=%v resp=%v, want %d", tc.path, err, resp, tc.want)
		}

		b, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(b), tc.code) || resp.Header.Get(gateway.HeaderUpstream) != "" {
			t.Errorf("%s: body %q, headers %v", tc.path, b, resp.Header)
		}

		if tc.want == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "7" {
			t.Errorf("Retry-After not passed through: %v", resp.Header)
		}
	}

	// Accepted: the node sees the gateway certificate, the minted token and
	// cid, the node id, the /ws path, and none of the forged headers nor
	// the browser credentials.
	h := http.Header{}
	h.Set("Cookie", "session=ok")
	h.Set("Authorization", "Basic c2VjcmV0")
	h.Set("Referer", "http://example.org/private")
	h.Set("Origin", "http://"+addr)
	h.Set(gateway.HeaderAccessToken, "forged")
	h.Set("X-Rx-Forged", "1")
	h.Set(gateway.HeaderUpstream, "evil:1")

	conn, resp, err := dial(t, nil, ws+"/nodes/roof/ws?v=1", h)
	if err != nil {
		t.Fatalf("dial through the gateway: %v", err)
	}

	// The 101 keeps only its handshake headers.
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Node-Private") != "" || resp.Header.Get("Sec-Websocket-Accept") == "" {
		t.Errorf("upgrade headers: %v", resp.Header)
	}

	ctx := context.Background()

	_, hello, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]string
	_ = json.Unmarshal(hello, &got)

	want := map[string]string{
		"path": "/ws", "query": "v=1", "peer": "gateway:hub.example.org", "token": "minted", "cid": "c1", "node": "roof",
		"forged": "", "upstream": "", "cookie": "", "authorization": "", "referer": "", "origin": "http://" + addr,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("node saw %s = %q, want %q (%v)", k, got[k], v, got)
		}
	}

	allowed := []string{
		"Connection", "Origin", "Sec-Websocket-Extensions", "Sec-Websocket-Key", "Sec-Websocket-Protocol", "Sec-Websocket-Version",
		"Upgrade", "User-Agent", "X-Rx-Access-Token", "X-Rx-Cid", "X-Rx-Node-Id",
	}
	for name := range strings.SplitSeq(got["headers"], ",") {
		if !slices.Contains(allowed, name) {
			t.Errorf("the node received header %s", name)
		}
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}

	if _, b, err := conn.Read(ctx); err != nil || string(b) != "ping" {
		t.Fatalf("echo = %q, %v", b, err)
	}

	// A node's own answer never reaches the browser on the hub origin:
	// no cookie, no content, a hub problem with the same status.
	_, resp, err = dial(t, nil, ws+"/nodes/evil/ws", h)
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("evil node: %v %v", err, resp)
	}

	b, _ := io.ReadAll(resp.Body)

	if len(resp.Header.Values("Set-Cookie")) != 0 || strings.Contains(string(b), "script") || !strings.Contains(string(b), "node_refused") ||
		resp.Header.Get("Content-Type") != "application/problem+json" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || resp.Header.Get("Cache-Control") != "no-store" ||
		resp.Header.Get("Server") != "" {
		t.Fatalf("node answer leaked: %v %q", resp.Header, b)
	}

	// An unreachable node is a bare 502.
	_, resp, _ = dial(t, nil, ws+"/nodes/gone/ws", h)
	if resp == nil || resp.StatusCode != http.StatusBadGateway || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unreachable node: %v", resp)
	}

	// The gateway logs at debug level, never with the access token nor the
	// browser credentials.
	logs := f.logs.String()
	if !strings.Contains(logs, "node media connection upgraded") || !strings.Contains(logs, "node refused the media connection") {
		t.Fatalf("no gateway debug log: %s", logs)
	}

	for _, secret := range []string{"minted", "session=ok", "c2VjcmV0"} {
		if strings.Contains(logs, secret) {
			t.Errorf("a secret reached the logs: %q", secret)
		}
	}
}

// The node dial runs NodeTLS: a peer without the node's URI SAN or failing
// the leaf check (pin, revocation) is never reached.
func TestGatewayNodeTransport(t *testing.T) {
	f := newFixture(t)
	f.check = func(leaf *x509.Certificate) error {
		if leaf.Subject.CommonName == "evil" {
			return pki.ErrRevoked
		}

		return nil
	}

	addr := freeAddr(t)
	f.start(t, gateway.Config{TLSMode: gateway.TLSOff, HTTPListen: addr, PublicURL: "http://" + addr})

	h := http.Header{}
	h.Set("Cookie", "session=ok")

	for _, id := range []string{"liar", "evil"} {
		_, resp, err := dial(t, nil, "ws://"+addr+"/nodes/"+id+"/ws", h)
		if err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: %v %v, want 502", id, err, resp)
		}
	}

	logs := f.logs.String()
	if !strings.Contains(logs, `"node_id":"liar"`) || !strings.Contains(logs, "liar.nodes.rx.internal") || !strings.Contains(logs, pki.ErrRevoked.Error()) {
		t.Errorf("dial failures not logged: %s", logs)
	}

	// NodeTLS failing (unknown node in the registry) is a 502 too.
	addr2 := freeAddr(t)
	o := f.options(gateway.Config{TLSMode: gateway.TLSOff, HTTPListen: addr2, PublicURL: "http://" + addr2})
	o.NodeTLS = func(context.Context, string) (*tls.Config, error) { return nil, errors.New("node_not_found") }
	startGateway(t, o)

	if _, resp, err := dial(t, nil, "ws://"+addr2+"/nodes/roof/ws", h); err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Errorf("NodeTLS error: %v %v", err, resp)
	}
}

// Stop closes the proxied WebSockets; StreamTimeout bounds their life.
func TestGatewayStreamLifetime(t *testing.T) {
	f := newFixture(t)
	h := http.Header{}
	h.Set("Cookie", "session=ok")

	addr := freeAddr(t)
	f.start(t, gateway.Config{TLSMode: gateway.TLSOff, HTTPListen: addr, PublicURL: "http://" + addr, StreamTimeout: 300 * time.Millisecond})

	conn, _, err := dial(t, nil, "ws://"+addr+"/nodes/roof/ws", h)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, _ = conn.Read(ctx) // hello

	if _, _, err := conn.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("the stream outlived stream_timeout: %v", err)
	}

	addr2 := freeAddr(t)

	g, err := gateway.New(f.options(gateway.Config{TLSMode: gateway.TLSOff, HTTPListen: addr2, PublicURL: "http://" + addr2}))
	if err != nil {
		t.Fatal(err)
	}

	if err := g.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	conn, _, err = dial(t, nil, "ws://"+addr2+"/nodes/roof/ws", h)
	if err != nil {
		t.Fatal(err)
	}

	_, _, _ = conn.Read(ctx)

	if err := g.Stop(); err != nil {
		t.Fatal(err)
	}

	if _, _, err := conn.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("the stream survived Stop: %v", err)
	}
}

// writePair writes a self-signed certificate for localhost.
func writePair(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()

	key, _ := pki.GenerateKey()

	cert, err := pki.SelfSigned(key, "localhost", "localhost:0", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	keyPEM, _ := pki.EncodeKeyPEM(key)
	certFile, keyFile = filepath.Join(dir, "pub.pem"), filepath.Join(dir, "pub.key")

	if err := os.WriteFile(certFile, pki.EncodeCertsPEM(cert.Certificate[0]), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	pool = x509.NewCertPool()
	pool.AddCert(cert.Leaf)

	return certFile, keyFile, pool
}

func TestGatewayOperatorCertificate(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile, pool := writePair(t, t.TempDir())

	addr, plain := freeAddr(t), freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	public := "https://localhost:" + port

	f.start(t, gateway.Config{
		TLSMode: gateway.TLSFiles, HTTPSListen: addr, HTTPListen: plain, CertFile: certFile, KeyFile: keyFile, PublicURL: public,
	})

	tlsConf := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConf, ForceAttemptHTTP2: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get("https://" + addr + "/") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	// HTTP/2 with TLS.
	if resp.StatusCode != 200 || resp.TLS == nil || resp.ProtoMajor != 2 {
		t.Fatalf("https hub: %d %s", resp.StatusCode, resp.Proto)
	}

	// The plain listener redirects to hub.url.
	resp, err = client.Get("http://" + plain + "/receiver?x=1") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != public+"/receiver?x=1" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// WebSockets go over HTTP/1.1.
	wsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf}}
	h := http.Header{}
	h.Set("Cookie", "session=ok")

	if _, _, err := dial(t, wsClient, "wss://"+addr+"/nodes/roof/ws", h); err != nil {
		t.Fatalf("wss through the gateway: %v", err)
	}

	// A missing key file is a startup error.
	o := f.options(gateway.Config{TLSMode: gateway.TLSFiles, HTTPSListen: freeAddr(t), CertFile: certFile, KeyFile: keyFile + ".missing", PublicURL: public})
	if _, err := gateway.New(o); err == nil {
		t.Fatal("New accepted a missing key file")
	}
}

// The internal mode certifies the host of hub.url with the hub CA.
func TestGatewayInternalCA(t *testing.T) {
	f := newFixture(t)
	addr := freeAddr(t)

	o := f.options(gateway.Config{TLSMode: gateway.TLSInternal, HTTPSListen: addr, PublicURL: "https://sdr.test"})
	if _, err := gateway.New(o); err == nil {
		t.Fatal("internal mode without the hub CA")
	}

	o.InternalCert = pki.NewServerSource(f.ca, "sdr.test", time.Now).Get
	startGateway(t, o)

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.ca.Pool(), ServerName: "sdr.test", MinVersion: tls.VersionTLS12}}}

	resp, err := client.Get("https://" + addr + "/") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK || resp.TLS.PeerCertificates[0].DNSNames[0] != "sdr.test" {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// ACME: the configuration is checked, the storage directory is created
// 0700, the plain listener answers HTTP-01 challenges and redirects the
// rest. No ACME CA is contacted.
func TestGatewayACME(t *testing.T) {
	f := newFixture(t)

	if _, err := gateway.New(f.options(gateway.Config{TLSMode: gateway.TLSACME, HTTPSListen: freeAddr(t), PublicURL: "https://sdr.example.org"})); err == nil {
		t.Fatal("ACME without a storage directory")
	}

	if _, err := gateway.New(f.options(gateway.Config{TLSMode: "letsencrypt", HTTPSListen: freeAddr(t), PublicURL: "https://sdr.example.org"})); err == nil {
		t.Fatal("unknown TLS mode accepted")
	}

	storage := filepath.Join(t.TempDir(), "acme")
	addr, plain := freeAddr(t), freeAddr(t)
	f.start(t, gateway.Config{
		TLSMode: gateway.TLSACME, HTTPSListen: addr, HTTPListen: plain, PublicURL: "https://sdr.example.org",
		StorageDir: storage, ACMECA: "https://acme.invalid/directory", ACMEEmail: "admin@example.org",
	})

	if info, err := os.Stat(storage); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("storage dir: %v %v", info, err)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get("http://" + plain + "/receiver") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://sdr.example.org/receiver" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = client.Get("http://" + plain + "/.well-known/acme-challenge/unknown") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusPermanentRedirect {
		t.Fatal("HTTP-01 challenges are redirected")
	}

	// TLS-ALPN-01 is offered on the TLS listener; a host other than the
	// one of hub.url gets no certificate.
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: "other.example.org", InsecureSkipVerify: true}) //nolint:gosec // test
	if err == nil {
		_ = conn.Close()
		t.Fatal("handshake for a foreign host")
	}
}
