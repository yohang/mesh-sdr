//go:build !nogateway

package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
)

// Gateway is the embedded Caddy instance of the hub.
type Gateway struct {
	o       Options
	binding string
	config  []byte
}

var (
	seq atomic.Int64
	// running guards the Caddy process singleton.
	runMu   sync.Mutex
	running bool
)

// Available reports whether this build embeds the gateway.
func Available() bool { return true }

// New builds the Caddy config and registers the module dependencies.
func New(o Options) (*Gateway, error) {
	if o.Hub == nil || o.NodeTLS == nil || o.Logger == nil {
		return nil, errors.New("gateway: hub, node TLS and logger are required")
	}

	g := &Gateway{o: o, binding: "gw" + strconv.FormatInt(seq.Add(1), 10)}

	cfg, err := buildConfig(o.Config, g.binding)
	if err != nil {
		return nil, err
	}

	g.config = cfg

	return g, nil
}

// Start creates the storage directory and loads the config: the listeners
// open and certificates are obtained in the background. Only one gateway
// runs per process.
func (g *Gateway) Start(ctx context.Context) error {
	runMu.Lock()
	defer runMu.Unlock()

	if running {
		return errors.New("gateway: already running in this process")
	}

	if err := os.MkdirAll(g.o.Config.StorageDir, 0o700); err != nil {
		return fmt.Errorf("gateway storage %s: %w", g.o.Config.StorageDir, err)
	}

	bindings.Store(g.binding, &binding{hub: g.o.Hub, nodeTLS: g.o.NodeTLS, logger: g.o.Logger})

	if err := caddy.Load(g.config, true); err != nil {
		bindings.Delete(g.binding)

		return fmt.Errorf("start gateway: %w", err)
	}

	running = true

	g.o.Logger.InfoContext(ctx, "gateway started", slog.String("component", "gateway"),
		slog.String("tls_mode", g.o.Config.TLSMode),
		slog.String("https_listen", g.o.Config.HTTPSListen), slog.String("http_listen", g.o.Config.HTTPListen))

	return nil
}

// Stop closes the listeners, waiting up to the grace period for requests in
// flight; proxied WebSockets are closed.
func (g *Gateway) Stop() error {
	runMu.Lock()
	defer runMu.Unlock()

	if !running {
		return nil
	}

	err := caddy.Stop()
	running = false

	bindings.Delete(g.binding)

	if err != nil {
		return fmt.Errorf("stop gateway: %w", err)
	}

	return nil
}
