package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"

	"github.com/caddyserver/caddy/v2"
)

// Driver is how the hub applies route changes.
type Driver string

const (
	// DriverLoad: embedded, the hub rebuilds the full JSON and calls caddy.Load.
	DriverLoad Driver = "embedded-load"
	// DriverAdminAPI: the hub talks to Caddy's admin API over a unix socket
	// (POST /id/node-routes/routes, DELETE /id/node-<id>). The admin API code
	// path is identical whether Caddy is in-process or a sidecar process, so
	// this measures the sidecar option.
	DriverAdminAPI Driver = "admin-api"
	// DriverDynamic: no Caddy call at all, the registry is read per request.
	DriverDynamic Driver = "dynamic"
)

type Gateway struct {
	opts   Options
	driver Driver
	mu     sync.Mutex
	nodes  []Node
	admin  *http.Client
	onNode func(Node, bool) // dynamic mode: registry mutation (hub side)
}

func New(opts Options, driver Driver, onNode func(Node, bool)) *Gateway {
	g := &Gateway{opts: opts, driver: driver, onNode: onNode}
	if opts.AdminSocket != "" {
		g.admin = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", opts.AdminSocket)
			},
		}}
	}
	return g
}

func (g *Gateway) Start(nodes []Node) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes = slices.Clone(nodes)
	if g.driver == DriverDynamic {
		for _, n := range nodes {
			g.onNode(n, true)
		}
	}
	cfg, err := g.opts.Config(g.nodes)
	if err != nil {
		return err
	}
	return caddy.Load(cfg, true)
}

func (g *Gateway) Stop() error { return caddy.Stop() }

func (g *Gateway) AddNode(n Node) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes = append(g.nodes, n)
	switch g.driver {
	case DriverDynamic:
		g.onNode(n, true)
		return nil
	case DriverAdminAPI:
		b, _ := json.Marshal(g.opts.NodeRoute(n))
		return g.call(http.MethodPost, "/id/node-routes/routes", b)
	default:
		return g.reload()
	}
}

func (g *Gateway) RemoveNode(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes = slices.DeleteFunc(g.nodes, func(n Node) bool { return n.ID == id })
	switch g.driver {
	case DriverDynamic:
		g.onNode(Node{ID: id}, false)
		return nil
	case DriverAdminAPI:
		return g.call(http.MethodDelete, "/id/node-"+id, nil)
	default:
		return g.reload()
	}
}

// Touch forces a reload of an unchanged config (e.g. cert rotation on disk).
func (g *Gateway) Touch() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reload()
}

func (g *Gateway) reload() error {
	cfg, err := g.opts.Config(g.nodes)
	if err != nil {
		return err
	}
	return caddy.Load(cfg, false)
}

func (g *Gateway) call(method, path string, body []byte) error {
	req, _ := http.NewRequest(method, "http://admin"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.admin.Do(req)
	if err != nil {
		return fmt.Errorf("admin %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("admin %s %s: %d %s", method, path, resp.StatusCode, b)
	}
	return nil
}
