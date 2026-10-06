package gateway

// Custom Caddy modules needed to embed Caddy cleanly in the hub.
//
// Caddy instantiates modules from JSON, so they cannot receive constructor
// dependencies. The only bridge is a process-wide binding table keyed by a name
// that appears in the JSON config. This is the one place where the embedded
// option clashes with the "no globals" rule of AGENTS.md: it must stay confined
// to the gateway infra adapter.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

func init() {
	caddy.RegisterModule(HubHandler{})
	caddy.RegisterModule(NodeUpstreams{})
	caddy.RegisterModule(SlogWriter{})
}

// NodeLookup resolves a node id to its node.listen address (hub registry).
type NodeLookup func(id string) (addr string, ok bool)

var bindings sync.Map // name -> http.Handler | NodeLookup | *slog.Logger

// Bind registers a dependency under a name referenced from the Caddy JSON.
func Bind(name string, v any) { bindings.Store(name, v) }

func lookup[T any](name string) (T, error) {
	var zero T
	v, ok := bindings.Load(name)
	if !ok {
		return zero, fmt.Errorf("no binding %q", name)
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("binding %q has type %T", name, v)
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// http.handlers.meshsdr_hub: serves the hub's own http.Handler (chi router in
// the real app) in-process, with no extra network hop.

type HubHandler struct {
	Name string `json:"name"`
	h    http.Handler
}

func (HubHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.meshsdr_hub", New: func() caddy.Module { return new(HubHandler) }}
}

func (m *HubHandler) Provision(caddy.Context) (err error) {
	m.h, err = lookup[http.Handler](m.Name)
	return err
}

func (m *HubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	m.h.ServeHTTP(w, r)
	return nil
}

// ---------------------------------------------------------------------------
// http.reverse_proxy.upstreams.meshsdr_nodes: dynamic upstream source reading
// the hub node registry. Node id comes from the {http.vars.rx_node} variable set
// by the route. Lets one static route serve every node: no reload on enroll,
// address change or removal.

type NodeUpstreams struct {
	Name string `json:"name"`
	find NodeLookup
}

func (NodeUpstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.reverse_proxy.upstreams.meshsdr_nodes", New: func() caddy.Module { return new(NodeUpstreams) }}
}

func (m *NodeUpstreams) Provision(caddy.Context) (err error) {
	m.find, err = lookup[NodeLookup](m.Name)
	return err
}

func (m *NodeUpstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	id, _ := caddyhttp.GetVar(r.Context(), "rx_node").(string)
	addr, ok := m.find(id)
	if !ok {
		return nil, fmt.Errorf("unknown node %q", id)
	}
	return []*reverseproxy.Upstream{{Dial: addr}}, nil
}

// ---------------------------------------------------------------------------
// caddy.logging.writers.slog: zap -> slog bridge. Caddy encodes each entry as
// JSON (encoder "json"), this writer decodes the line and re-emits it on the
// injected *slog.Logger, so Caddy logs land in the app's single stderr stream
// with the app's format, level filter and "component" attribute.

type SlogWriter struct {
	Name string `json:"name"`
}

func (SlogWriter) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "caddy.logging.writers.slog", New: func() caddy.Module { return new(SlogWriter) }}
}

func (w SlogWriter) String() string    { return "slog:" + w.Name }
func (w SlogWriter) WriterKey() string { return "slog:" + w.Name }

func (w SlogWriter) OpenWriter() (io.WriteCloser, error) {
	l, err := lookup[*slog.Logger](w.Name)
	if err != nil {
		return nil, err
	}
	return &slogSink{log: l}, nil
}

type slogSink struct{ log *slog.Logger }

func (s *slogSink) Close() error { return nil }

func (s *slogSink) Write(p []byte) (int, error) {
	sc := bufio.NewScanner(bytes.NewReader(p))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			s.log.Warn("undecodable caddy log line", slog.String("line", sc.Text()))
			continue
		}
		level := slog.LevelInfo
		switch e["level"] {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error", "dpanic", "panic", "fatal":
			level = slog.LevelError
		}
		msg, _ := e["msg"].(string)
		logger, _ := e["logger"].(string)
		var ts time.Time
		if f, ok := e["ts"].(float64); ok {
			ts = time.Unix(0, int64(f*1e9))
		}
		component := "gateway.caddy"
		if logger != "" {
			component += "." + logger
		} else {
			component += ".stdlib" // the "sink" (stdlib log package, redirected by Caddy)
		}
		attrs := []slog.Attr{slog.String("component", component)}
		for k, v := range e {
			switch k {
			case "level", "msg", "logger", "ts":
				continue
			}
			attrs = append(attrs, slog.Any(k, v))
		}
		ctx := context.Background()
		if !s.log.Enabled(ctx, level) {
			continue
		}
		rec := slog.NewRecord(ts, level, msg, 0) // keep Caddy's timestamp
		rec.AddAttrs(attrs...)
		_ = s.log.Handler().Handle(ctx, rec)
	}
	return len(p), nil
}

var (
	_ caddyhttp.MiddlewareHandler = (*HubHandler)(nil)
	_ reverseproxy.UpstreamSource = (*NodeUpstreams)(nil)
	_ caddy.WriterOpener          = (*SlogWriter)(nil)
)
