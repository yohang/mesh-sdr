//go:build !nogateway

package gateway_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/grid/infra/gateway"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

// These tests start the real Caddy, a process singleton: they never run in
// parallel. They are the contract test of the custom modules, to run on
// every Caddy upgrade (ADR 0002).

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
	gwClient *pki.ClientSource
	logs     *syncBuffer
}

// newFixture starts a fake node "roof": mTLS with the hub CA, /ws echoes
// what it received as its first message.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	certPEM, keyPEM, err := pki.GenerateCA("test", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	ca, _ := pki.ParseCA(certPEM, keyPEM)
	key, _ := pki.GenerateKey()
	csr, _ := pki.CreateNodeCSR(key, "roof", "127.0.0.1:0")

	der, err := ca.SignNodeCSR(csr, "roof", "127.0.0.1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	node := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, id, _ := pki.PeerIdentity(r)

		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{"rx.v1"}})
		if err != nil {
			return
		}

		defer func() { _ = ws.CloseNow() }()

		hello, _ := json.Marshal(map[string]string{
			"path": r.URL.Path, "peer": kind + ":" + id, "token": r.Header.Get(gateway.HeaderAccessToken),
			"cid": r.Header.Get(gateway.HeaderCID), "node": r.Header.Get(gateway.HeaderNodeID),
			"forged": r.Header.Get("X-Rx-Forged"), "upstream": r.Header.Get(gateway.HeaderUpstream),
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
	node.TLS = pki.NodeServerConfig(pki.NewCertHolder(tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}), ca.Pool(), nil)
	node.StartTLS()
	t.Cleanup(node.Close)

	return &fixture{ca: ca, node: node, gwClient: pki.NewClientSource(ca, pki.KindGateway, "hub.example.org", time.Now), logs: &syncBuffer{}}
}

// hub is a fake hub router: "/" answers "hub", the authz admits node "roof"
// with the cookie session=ok.
func (f *fixture) hub() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hub") })
	mux.HandleFunc(gateway.AuthzPath, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("node") == "offline":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":"node_offline"}`)
		case r.URL.Query().Get("node") != "roof":
			w.WriteHeader(http.StatusNotFound)
		case r.Header.Get(gateway.HeaderAccessToken) != "":
			// The forged header must never reach the authz.
			w.WriteHeader(http.StatusTeapot)
		default:
			if c, err := r.Cookie("session"); err != nil || c.Value != "ok" {
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			w.Header().Set(gateway.HeaderAccessToken, "minted")
			w.Header().Set(gateway.HeaderCID, "c1")
			w.Header().Set(gateway.HeaderUpstream, strings.TrimPrefix(f.node.URL, "https://"))
			w.WriteHeader(http.StatusNoContent)
		}
	})

	return mux
}

func (f *fixture) start(t *testing.T, c gateway.Config) {
	t.Helper()

	c.StorageDir = t.TempDir()
	c.StreamCloseDelay, c.StreamTimeout, c.LogLevel = time.Hour, time.Hour, "debug"

	g, err := gateway.New(gateway.Options{
		Config: c, Hub: f.hub(),
		NodeTLS: func(_ context.Context, id string) (*tls.Config, error) {
			return pki.HubDialConfig(f.gwClient, f.ca.Pool(), id, nil), nil
		},
		Logger: slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
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

func TestGatewayPlainHTTP(t *testing.T) {
	f := newFixture(t)
	addr := freeAddr(t)
	f.start(t, gateway.Config{TLSMode: gateway.TLSOff, HTTPListen: addr, PublicURL: "http://" + addr})

	base := "http://" + addr

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != 200 || string(body) != "hub" || resp.Header.Get("Server") != "" {
		t.Fatalf("hub: %d %q server=%q", resp.StatusCode, body, resp.Header.Get("Server"))
	}

	for _, path := range []string{gateway.AuthzPath + "?node=roof", "/nodes/x/ws", "/nodes/roof/other"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}

	ws := "ws://" + addr

	// Authz answers pass through, with their body; no upgrade happens.
	cases := []struct {
		path   string
		cookie string
		forged bool
		want   int
	}{
		{"/nodes/shack/ws", "ok", false, http.StatusNotFound},
		{"/nodes/offline/ws", "ok", false, http.StatusServiceUnavailable},
		{"/nodes/roof/ws", "", false, http.StatusUnauthorized},
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
	}

	_, resp, _ = dial(t, nil, ws+"/nodes/offline/ws", nil)
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "node_offline") {
		t.Fatalf("authz body not passed through: %q", b)
	}

	// Accepted: the node sees the gateway certificate, the minted token and
	// cid, the node id, the /ws path, and none of the forged headers.
	h := http.Header{}
	h.Set("Cookie", "session=ok")
	h.Set(gateway.HeaderAccessToken, "forged")
	h.Set("X-Rx-Forged", "1")
	h.Set(gateway.HeaderUpstream, "evil:1")

	conn, _, err := dial(t, nil, ws+"/nodes/roof/ws", h)
	if err != nil {
		t.Fatalf("dial through the gateway: %v", err)
	}

	ctx := context.Background()

	_, hello, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]string
	_ = json.Unmarshal(hello, &got)

	want := map[string]string{
		"path": "/ws", "peer": "gateway:hub.example.org", "token": "minted", "cid": "c1", "node": "roof", "forged": "", "upstream": "",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("node saw %s = %q, want %q (%v)", k, got[k], v, got)
		}
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}

	if _, b, err := conn.Read(ctx); err != nil || string(b) != "ping" {
		t.Fatalf("echo = %q, %v", b, err)
	}

	// Caddy logs reach the injected logger, with a gateway.caddy component.
	if !strings.Contains(f.logs.String(), `"component":"gateway.caddy`) {
		t.Fatalf("no Caddy log through slog: %s", f.logs.String())
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

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != 200 || resp.TLS == nil {
		t.Fatalf("https hub: %d", resp.StatusCode)
	}

	// The plain listener redirects to hub.url.
	resp, err = client.Get("http://" + plain + "/receiver?x=1")
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != public+"/receiver?x=1" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	h := http.Header{}
	h.Set("Cookie", "session=ok")

	if _, _, err := dial(t, client, "wss://"+addr+"/nodes/roof/ws", h); err != nil {
		t.Fatalf("wss through the gateway: %v", err)
	}
}

// The internal issuer certifies the host of hub.url with a local CA kept in
// the storage directory.
func TestGatewayInternalCA(t *testing.T) {
	f := newFixture(t)
	addr := freeAddr(t)
	storage := t.TempDir()

	g, err := gateway.New(gateway.Options{
		Config: gateway.Config{
			TLSMode: gateway.TLSInternal, HTTPSListen: addr, PublicURL: "https://sdr.test", StorageDir: storage,
			StreamCloseDelay: time.Hour, StreamTimeout: time.Hour,
		},
		Hub: f.hub(), NodeTLS: func(context.Context, string) (*tls.Config, error) { return nil, nil },
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := g.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = g.Stop() })

	var resp *http.Response

	deadline := time.Now().Add(10 * time.Second)

	for {
		root, rerr := os.ReadFile(filepath.Join(storage, "pki", "authorities", "local", "root.crt"))

		if rerr == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(root)

			client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "sdr.test", MinVersion: tls.VersionTLS12}}}

			resp, err = client.Get("https://" + addr + "/")
			if err == nil {
				break
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("no certificate from the internal CA: %v %v", rerr, err)
		}

		time.Sleep(50 * time.Millisecond)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestGatewaySingleton(t *testing.T) {
	f := newFixture(t)
	addr := freeAddr(t)
	f.start(t, gateway.Config{TLSMode: gateway.TLSOff, HTTPListen: addr, PublicURL: "http://" + addr})

	g, err := gateway.New(gateway.Options{
		Config: gateway.Config{TLSMode: gateway.TLSOff, HTTPListen: freeAddr(t), PublicURL: "http://x", StorageDir: t.TempDir()},
		Hub:    f.hub(), NodeTLS: func(context.Context, string) (*tls.Config, error) { return nil, nil },
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := g.Start(context.Background()); err == nil {
		_ = g.Stop()
		t.Fatal("a second gateway started in the same process")
	}
}
