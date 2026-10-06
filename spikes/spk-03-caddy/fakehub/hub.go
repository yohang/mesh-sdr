// Package fakehub stands in for the hub: a node registry (the source of truth
// the gateway is driven from), the forward-auth endpoint that mints EdDSA access
// tokens (TECHNICAL_SPEC §5.8, §5.16) and a trivial "UI" handler.
package fakehub

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const HubURL = "https://hub.spike.local"

type NodeEntry struct {
	ID      string
	Addr    string // host:port of node.listen
	Online  bool
	Enabled bool
}

// Registry is the hub-owned node registry. In "dynamic" gateway mode the
// gateway reads it directly (no Caddy reload on change).
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]NodeEntry
}

func NewRegistry() *Registry { return &Registry{nodes: map[string]NodeEntry{}} }

func (r *Registry) Put(n NodeEntry)  { r.mu.Lock(); r.nodes[n.ID] = n; r.mu.Unlock() }
func (r *Registry) Delete(id string) { r.mu.Lock(); delete(r.nodes, id); r.mu.Unlock() }
func (r *Registry) Get(id string) (NodeEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	return n, ok
}

type Hub struct {
	Reg      *Registry
	Pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	log      *slog.Logger
	AuthzHit atomic.Int64
	seq      atomic.Int64
}

func New(reg *Registry, log *slog.Logger) *Hub {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return &Hub{Reg: reg, Pub: pub, priv: priv, log: log.With(slog.String("component", "fakehub.authz"))}
}

// Handler is what the hub serves: UI root + internal authz endpoint.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/gateway/authz", h.authz)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hub ui")) })
	mux.HandleFunc("GET /api/v1/ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("pong")) })
	return mux
}

func (h *Hub) authz(w http.ResponseWriter, r *http.Request) {
	h.AuthzHit.Add(1)
	id := r.URL.Query().Get("node")
	n, ok := h.Reg.Get(id)
	switch {
	case !ok || !n.Enabled:
		http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
		return
	case !n.Online:
		http.Error(w, `{"code":"node_offline"}`, http.StatusServiceUnavailable)
		return
	}
	c, err := r.Cookie("session")
	if err != nil || c.Value != "valid" {
		http.Error(w, `{"code":"login_required"}`, http.StatusUnauthorized)
		return
	}
	if r.Header.Get("X-Rx-Access-Token") != "" {
		// must never happen: the gateway strips client-supplied X-Rx-* headers
		h.log.Error("client-supplied access token reached authz")
	}
	cid := fmt.Sprintf("c%d", h.seq.Add(1))
	now := time.Now()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": HubURL, "aud": "rx-node:" + id, "sub": "u-alice", "cid": cid,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(300 * time.Second).Unix(),
	}).SignedString(h.priv)
	if err != nil {
		http.Error(w, "sign", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Rx-Access-Token", tok)
	w.Header().Set("X-Rx-Cid", cid)
	w.WriteHeader(http.StatusNoContent)
}
