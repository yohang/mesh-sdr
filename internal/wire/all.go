package wire

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

// EnsureCA creates the hub CA at certPath and keyPath when both are absent
// (first start of the all role, TECHNICAL_SPEC §7.4); it never replaces
// one. It reports whether it created them.
func EnsureCA(certPath, keyPath string, now time.Time) (bool, error) {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)

	switch {
	case certErr == nil && keyErr == nil:
		return false, nil
	case !errors.Is(certErr, fs.ErrNotExist) || !errors.Is(keyErr, fs.ErrNotExist):
		return false, fmt.Errorf("hub CA: %s and %s must both exist or both be absent", certPath, keyPath)
	}

	if _, err := pki.CreateCA(certPath, keyPath, now); err != nil {
		return false, err
	}

	return true, nil
}

// AllProcess is the all role: the hub and its local node in one process
// (GRID-003, GRID-014). The local node is a normal grid node, declared in
// the hub with origin config, reached over loopback mTLS by the control
// channel and through the gateway like any other node.
type AllProcess struct {
	hub     *Process
	g       *hubGrid
	nodeCfg config.Node
	logger  *slog.Logger
	root    *slog.Logger
	now     func() time.Time
}

// All builds the all role. The hub CA must exist (EnsureCA).
func All(ctx context.Context, hubCfg config.Hub, origins config.Origins, nodeCfg config.Node, logger *slog.Logger, adapter *db.DB) (*AllProcess, error) {
	id, err := griddomain.NewNodeID(nodeCfg.Node.ID)
	if err != nil {
		return nil, fmt.Errorf("node.id: %w", err)
	}

	if hubCfg.TLS.CACert == "" {
		return nil, errors.New("the all role needs the hub CA (tls.ca_cert)")
	}

	if _, ok := hubCfg.Nodes[id.String()]; ok {
		return nil, fmt.Errorf("nodes.%s: this id is the local node of the all role (node.id)", id)
	}

	url, err := localURL(nodeCfg.Node.Listen)
	if err != nil {
		return nil, err
	}

	hubID, err := HubID(hubCfg.Hub.URL)
	if err != nil {
		return nil, err
	}

	if nodeCfg.HubTrust.HubIdentity == "" {
		nodeCfg.HubTrust.HubIdentity = hubID
	}

	nodes := make(map[string]config.ConfigNode, len(hubCfg.Nodes)+1)
	for k, v := range hubCfg.Nodes {
		nodes[k] = v
	}

	nodes[id.String()] = config.ConfigNode{URL: url}
	hubCfg.Nodes = nodes

	hub, g, err := newHub(ctx, hubCfg, origins, logger, adapter, time.Now, gridapp.DefaultTimings())
	if err != nil {
		return nil, err
	}

	return &AllProcess{hub: hub, g: g, nodeCfg: nodeCfg, logger: component(logger, "wire.all"), root: logger, now: time.Now}, nil
}

// Hub returns the hub process.
func (a *AllProcess) Hub() *Process { return a.hub }

// localURL is the URL the hub dials for a node listening on listen: a
// wildcard host becomes the loopback address.
func localURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("node.listen: %w", err)
	}

	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}

	return "https://" + net.JoinHostPort(host, port), nil
}

// Run starts the hub (declaring the local node), enrolls the local node
// in-process when needed, then serves the hub and the node until ctx is
// done or one of them stops.
func (a *AllProcess) Run(ctx context.Context) error {
	if err := a.hub.runStartup(ctx); err != nil {
		return err
	}

	runNode := true

	if err := a.enrollLocal(ctx); err != nil {
		if !errors.Is(err, griddomain.ErrNodeRevoked) {
			return err
		}

		runNode = false

		a.logger.ErrorContext(ctx, "the local node is revoked: it is not started; issue a new enrollment token "+
			"(meshsdr hub node token "+a.nodeCfg.Node.ID+") to re-enroll it at the next start")
	}

	var node *Process

	if runNode {
		var err error
		if node, err = Node(a.nodeCfg, a.root, a.now()); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var g errgroup.Group

	for _, p := range []*Process{a.hub, node} {
		if p != nil {
			g.Go(func() error {
				defer cancel()

				return p.Run(ctx)
			})
		}
	}

	return g.Wait()
}

// enrollLocal issues the local node certificate from the hub CA when the
// node files hold none valid for the enrolled node.
func (a *AllProcess) enrollLocal(ctx context.Context) error {
	cfg := a.nodeCfg

	paths := enroll.Paths{Key: cfg.TLS.Key, Cert: cfg.TLS.Cert, CA: cfg.HubTrust.CACert}
	if cfg.HubTrust.CACert == a.g.caPath {
		paths.CA = ""
	}

	info, issued, err := enroll.Local(ctx, enroll.LocalOptions{
		ID: cfg.Node.ID, Listen: cfg.Node.Listen, CA: a.g.ca, Nodes: a.g.nodes, Revocations: a.g.revocations,
		Paths: paths, Now: a.now,
	})
	if err != nil {
		return err
	}

	if issued {
		a.logger.InfoContext(ctx, "local node enrolled in-process", slog.String("node_id", cfg.Node.ID),
			slog.String("cert_serial", info.Serial()))
	}

	return nil
}
