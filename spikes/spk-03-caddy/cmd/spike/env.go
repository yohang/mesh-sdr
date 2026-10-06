package main

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yohang/mesh-sdr/spikes/spk-03-caddy/fakehub"
	"github.com/yohang/mesh-sdr/spikes/spk-03-caddy/fakenode"
	"github.com/yohang/mesh-sdr/spikes/spk-03-caddy/gateway"
	"github.com/yohang/mesh-sdr/spikes/spk-03-caddy/pki"
)

const (
	gwPort = 18443
	domain = "localhost"
)

// countingHandler counts records per component (to prove the zap->slog bridge).
type countingHandler struct {
	slog.Handler
	min    slog.Level // records below this are counted but not printed
	mu     *sync.Mutex
	counts map[string]int
	sample *atomic.Value
}

func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h countingHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			h.mu.Lock()
			h.counts[a.Value.String()]++
			h.mu.Unlock()
			if h.sample.Load() == nil && len(a.Value.String()) > 14 && a.Value.String()[:14] == "gateway.caddy." {
				h.sample.Store(fmt.Sprintf("level=%s msg=%q component=%s", r.Level, r.Message, a.Value))
			}
		}
		return true
	})
	if r.Level < h.min {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}
func (h countingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return countingHandler{h.Handler.WithAttrs(as), h.min, h.mu, h.counts, h.sample}
}
func (h countingHandler) WithGroup(n string) slog.Handler {
	return countingHandler{h.Handler.WithGroup(n), h.min, h.mu, h.counts, h.sample}
}

type env struct {
	dir     string
	ca      *pki.CA
	gwLeaf  pki.Leaf
	op      pki.Leaf
	nodes   map[string]*fakenode.Node
	reg     *fakehub.Registry
	hub     *fakehub.Hub
	hubSock string
	hubSrv  *http.Server
	gw      *gateway.Gateway
	opts    gateway.Options
	br      browser
	log     *slog.Logger
	counts  map[string]int
	countMu *sync.Mutex
	sample  *atomic.Value
}

func newEnv(base string, log *slog.Logger, counts map[string]int, mu *sync.Mutex, sample *atomic.Value) (*env, error) {
	dir, err := os.MkdirTemp(base, "spk03-")
	if err != nil {
		return nil, err
	}
	e := &env{dir: dir, nodes: map[string]*fakenode.Node{}, reg: fakehub.NewRegistry(), log: log, counts: counts, countMu: mu, sample: sample}
	if e.ca, err = pki.NewCA(dir); err != nil {
		return nil, err
	}
	if e.gwLeaf, err = e.ca.Gateway("hub1"); err != nil {
		return nil, err
	}
	if e.op, err = e.ca.OperatorCert(domain); err != nil {
		return nil, err
	}
	e.hub = fakehub.New(e.reg, log)
	for _, id := range []string{"roof", "shack", "attic"} {
		leaf, err := e.ca.Node(id)
		if err != nil {
			return nil, err
		}
		n, err := fakenode.Start(id, leaf.TLS, e.ca.Pool, e.hub.Pub, fakehub.HubURL, log)
		if err != nil {
			return nil, err
		}
		e.nodes[id] = n
	}
	// hub internal listener on a unix socket (forward-auth target)
	e.hubSock = filepath.Join(dir, "hub.sock")
	ln, err := net.Listen("unix", e.hubSock)
	if err != nil {
		return nil, err
	}
	e.hubSrv = &http.Server{Handler: e.hub.Handler()}
	go func() { _ = e.hubSrv.Serve(ln) }()

	gateway.Bind("hub", e.hub.Handler())
	gateway.Bind("nodes", gateway.NodeLookup(func(id string) (string, bool) {
		n, ok := e.reg.Get(id)
		if !ok || !n.Enabled {
			return "", false
		}
		return n.Addr, true
	}))
	gateway.Bind("log", log.With(slog.String("subsystem", "gateway")))
	return e, nil
}

func (e *env) startGateway(driver gateway.Driver, tlsMode gateway.TLSMode, delay time.Duration, initial ...string) error {
	routing := gateway.RoutingPerNode
	if driver == gateway.DriverDynamic {
		routing = gateway.RoutingDynamic
	}
	admin := ""
	if driver == gateway.DriverAdminAPI {
		admin = filepath.Join(e.dir, "caddy-admin.sock")
	}
	e.opts = gateway.Options{
		Listen: fmt.Sprintf(":%d", gwPort), HTTPSPort: gwPort,
		Routing: routing, TLS: tlsMode, Domain: domain,
		OperatorCert: e.op.CertFile, OperatorKey: e.op.KeyFile,
		StorageDir: filepath.Join(e.dir, "caddy-data"), AdminSocket: admin,
		HubHandlerName: "hub", HubAuthzDial: "unix/" + e.hubSock, NodeLookupName: "nodes", LoggerName: "log",
		CAFile: e.ca.CAFile(), GatewayCert: e.gwLeaf.CertFile, GatewayKey: e.gwLeaf.KeyFile,
		StreamCloseDelay: delay, StreamTimeout: 24 * time.Hour,
	}
	e.gw = gateway.New(e.opts, driver, func(n gateway.Node, add bool) {})
	var ns []gateway.Node
	for _, id := range initial {
		e.reg.Put(fakehub.NodeEntry{ID: id, Addr: e.nodes[id].Addr, Online: true, Enabled: true})
		ns = append(ns, gateway.Node{ID: id, Addr: e.nodes[id].Addr})
	}
	if err := e.gw.Start(ns); err != nil {
		return err
	}
	pool := x509.NewCertPool()
	switch tlsMode {
	case gateway.TLSFiles:
		pool.AppendCertsFromPEM(mustRead(e.op.CertFile))
	case gateway.TLSInternal:
		// Caddy's local CA root appears in storage once the pki app has started
		root := filepath.Join(e.opts.StorageDir, "pki", "authorities", "local", "root.crt")
		deadline := time.Now().Add(10 * time.Second)
		for {
			if b, err := os.ReadFile(root); err == nil {
				pool.AppendCertsFromPEM(b)
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("internal CA root not found")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	e.br = browser{host: domain, addr: fmt.Sprintf("127.0.0.1:%d", gwPort), pool: pool}
	return nil
}

// addNode / removeNode mimic the hub: registry first (authz), then the gateway.
func (e *env) addNode(id string) (time.Duration, error) {
	e.reg.Put(fakehub.NodeEntry{ID: id, Addr: e.nodes[id].Addr, Online: true, Enabled: true})
	t0 := time.Now()
	err := e.gw.AddNode(gateway.Node{ID: id, Addr: e.nodes[id].Addr})
	return time.Since(t0), err
}

func (e *env) removeNode(id string) (time.Duration, error) {
	e.reg.Delete(id)
	t0 := time.Now()
	err := e.gw.RemoveNode(id)
	return time.Since(t0), err
}

func (e *env) stopGateway() {
	_ = e.gw.Stop()
	for _, id := range []string{"roof", "shack", "attic"} {
		e.reg.Delete(id)
	}
}

func (e *env) close() {
	for _, n := range e.nodes {
		n.Close()
	}
	_ = e.hubSrv.Close()
	_ = os.RemoveAll(e.dir)
}

func mustRead(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return b
}
