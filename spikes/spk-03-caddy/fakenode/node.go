// Package fakenode is a minimal node agent: an mTLS-only WebSocket echo server
// on /ws that accepts only peers with a gateway URI SAN and verifies the
// hub-minted access token offline (EdDSA), as in TECHNICAL_SPEC §4.3 / §5.8.
package fakenode

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
)

// Hello is the first message the node sends on every media WS: it lets the
// test client see what the node received through the gateway.
type Hello struct {
	Node          string `json:"node"`
	Sub           string `json:"sub"`
	Cid           string `json:"cid"`
	HeaderNodeID  string `json:"x_rx_node_id"`
	PeerURISAN    string `json:"peer_uri_san"`
	ForgedTokenIn bool   `json:"forged_token_seen"`
}

type Node struct {
	ID     string
	Addr   string
	Open   atomic.Int64
	srv    *http.Server
	ln     net.Listener
	pub    ed25519.PublicKey
	hubURL string
	log    *slog.Logger
}

func Start(id string, cert tls.Certificate, caPool *x509.CertPool, pub ed25519.PublicKey, hubURL string, log *slog.Logger) (*Node, error) {
	n := &Node{ID: id, pub: pub, hubURL: hubURL, log: log.With(slog.String("component", "fakenode."+id))}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		NextProtos:   []string{"http/1.1"},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		return nil, err
	}
	n.ln, n.Addr = ln, ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", n.ws)
	n.srv = &http.Server{Handler: mux, ErrorLog: slog.NewLogLogger(n.log.Handler(), slog.LevelWarn)}
	go func() { _ = n.srv.Serve(ln) }()
	return n, nil
}

func (n *Node) Close() { _ = n.srv.Close() }

func peerURISAN(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	for _, u := range r.TLS.PeerCertificates[0].URIs {
		return u.String()
	}
	return ""
}

func (n *Node) ws(w http.ResponseWriter, r *http.Request) {
	san := peerURISAN(r)
	if !strings.HasPrefix(san, "urn:rx:gateway:") {
		http.Error(w, "gateway SAN required", http.StatusForbidden)
		return
	}
	raw := r.Header.Get("X-Rx-Access-Token")
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) { return n.pub, nil },
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithAudience("rx-node:"+n.ID),
		jwt.WithIssuer(n.hubURL),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		n.log.Warn("token rejected", slog.Any("error", err))
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"rx.v1"}, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	n.Open.Add(1)
	defer n.Open.Add(-1)
	sub, _ := claims["sub"].(string)
	cid, _ := claims["cid"].(string)
	hello, _ := json.Marshal(Hello{Node: n.ID, Sub: sub, Cid: cid, HeaderNodeID: r.Header.Get("X-Rx-Node-Id"), PeerURISAN: san, ForgedTokenIn: raw == "forged"})
	ctx := context.Background()
	if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
		return
	}
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				n.log.Debug("ws closed", slog.String("reason", fmt.Sprint(err)))
			}
			return
		}
		if err := c.Write(ctx, typ, b); err != nil {
			return
		}
	}
}
