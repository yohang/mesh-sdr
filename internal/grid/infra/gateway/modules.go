//go:build !nogateway

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"

	// The gateway module set (ADR 0012); modules/standard is not linked.
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/rewrite"
	_ "github.com/caddyserver/caddy/v2/modules/caddypki"
	_ "github.com/caddyserver/caddy/v2/modules/caddytls"
	_ "github.com/caddyserver/caddy/v2/modules/filestorage"
	_ "github.com/caddyserver/caddy/v2/modules/logging"
)

// Caddy variables set by the authz module.
const (
	varNode     = "rx_node"
	varUpstream = "rx_upstream"
)

var nodePath = regexp.MustCompile(`^/nodes/([a-z0-9][a-z0-9-]{1,62})/ws$`)

// binding holds the dependencies of the custom modules of one Gateway.
type binding struct {
	hub     http.Handler
	nodeTLS func(ctx context.Context, nodeID string) (*tls.Config, error)
	logger  *slog.Logger
}

// bindings is the binding table: Caddy modules are built from JSON, so this
// package-level map is their only way to reach their dependencies (ADR 0002
// decision 5). Keys are unique per Gateway.
var bindings sync.Map

func lookup(name string) (*binding, error) {
	v, ok := bindings.Load(name)
	if !ok {
		return nil, fmt.Errorf("gateway: no binding %q", name)
	}

	return v.(*binding), nil //nolint:forcetypeassert // only *binding is stored
}

// Module registration is a Caddy requirement (init-time global registry),
// confined to this adapter with the binding table.
func init() { //nolint:gochecknoinits // Caddy module registry
	caddy.RegisterModule(HubHandler{})
	caddy.RegisterModule(NodeAuthz{})
	caddy.RegisterModule(NodeUpstreams{})
	caddy.RegisterModule(NodeTransport{})
	caddy.RegisterModule(SlogWriter{})
}

// HubHandler (http.handlers.meshsdr_hub) serves the hub router in-process.
type HubHandler struct {
	Binding string `json:"binding"`
	b       *binding
}

// CaddyModule implements caddy.Module.
func (HubHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.meshsdr_hub", New: func() caddy.Module { return new(HubHandler) }}
}

// Provision implements caddy.Provisioner.
func (m *HubHandler) Provision(caddy.Context) (err error) {
	m.b, err = lookup(m.Binding)

	return err
}

func (m *HubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	w.Header().Del("Server")
	m.b.hub.ServeHTTP(w, r)

	return nil
}

// NodeAuthz (http.handlers.meshsdr_node_authz) is the forward auth of the
// node route, run in-process against the hub router (ADR 0012).
type NodeAuthz struct {
	Binding string `json:"binding"`
	b       *binding
}

// CaddyModule implements caddy.Module.
func (NodeAuthz) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.meshsdr_node_authz", New: func() caddy.Module { return new(NodeAuthz) }}
}

// Provision implements caddy.Provisioner.
func (m *NodeAuthz) Provision(caddy.Context) (err error) {
	m.b, err = lookup(m.Binding)

	return err
}

// maxAuthzBody bounds the recorded authz answer.
const maxAuthzBody = 64 << 10

func (m *NodeAuthz) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	match := nodePath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		w.Header().Del("Server")
		w.WriteHeader(http.StatusNotFound)

		return nil
	}

	id := match[1]

	w.Header().Del("Server")

	for name := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Rx-") {
			r.Header.Del(name)
		}
	}

	sub := r.Clone(r.Context())
	sub.Method = http.MethodGet
	sub.URL = &url.URL{Path: AuthzPath, RawQuery: url.Values{"node": {id}}.Encode()}
	sub.RequestURI = sub.URL.RequestURI()
	sub.Body = http.NoBody
	sub.ContentLength = 0

	for _, h := range []string{"Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions"} {
		sub.Header.Del(h)
	}

	rec := &recorder{header: http.Header{}}
	m.b.hub.ServeHTTP(rec, sub)

	if rec.status/100 != 2 {
		for k, v := range rec.header {
			if !strings.HasPrefix(k, "X-Rx-") {
				w.Header()[k] = v
			}
		}

		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())

		return nil
	}

	upstream := rec.header.Get(HeaderUpstream)
	if upstream == "" {
		return caddyhttp.Error(http.StatusBadGateway, errors.New("authz answered without an upstream"))
	}

	// Nodes never see the browser's credentials (§5.8): only the WebSocket
	// handshake, Origin and User-Agent go through, with the token and cid
	// set by the authz.
	forwarded := http.Header{}

	for _, k := range forwardedRequestHeaders {
		if v, ok := r.Header[k]; ok {
			forwarded[k] = v
		}
	}

	forwarded.Set(HeaderAccessToken, rec.header.Get(HeaderAccessToken))
	forwarded.Set(HeaderCID, rec.header.Get(HeaderCID))
	r.Header = forwarded

	caddyhttp.SetVar(r.Context(), varNode, id)
	caddyhttp.SetVar(r.Context(), varUpstream, upstream)

	return next.ServeHTTP(&nodeResponse{ResponseWriter: w, r: r}, r)
}

// forwardedRequestHeaders are the browser headers a node receives.
var forwardedRequestHeaders = []string{
	"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Protocol",
	"Sec-Websocket-Extensions", "Origin", "User-Agent",
}

// upgradeResponseHeaders are the node response headers the browser
// receives on a successful upgrade.
var upgradeResponseHeaders = []string{
	"Connection", "Upgrade", "Sec-Websocket-Accept", "Sec-Websocket-Protocol", "Sec-Websocket-Extensions",
}

// nodeResponse filters what a node answers on the hub origin: a successful
// upgrade keeps only its handshake headers; any other answer is replaced
// by a hub problem with the same status, so no node content (cookies,
// scripts, documents) is ever served on the hub origin.
type nodeResponse struct {
	http.ResponseWriter
	r       *http.Request
	done    bool
	swallow bool
}

// Unwrap lets http.ResponseController reach Hijack and Flush.
func (n *nodeResponse) Unwrap() http.ResponseWriter { return n.ResponseWriter }

func (n *nodeResponse) upgrade(code int) bool {
	if code == http.StatusSwitchingProtocols {
		return true
	}

	// WebSocket over HTTP/2 extended CONNECT answers 200 (Caddy).
	_, h2 := caddyhttp.GetVar(n.r.Context(), "extended_connect_websocket_body").(io.ReadCloser)

	return code == http.StatusOK && h2
}

func (n *nodeResponse) WriteHeader(code int) {
	if n.done {
		return
	}

	h := n.ResponseWriter.Header()

	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		clear(h)
		n.ResponseWriter.WriteHeader(code)

		return
	}

	n.done = true

	if n.upgrade(code) {
		for k := range h {
			if !slices.Contains(upgradeResponseHeaders, http.CanonicalHeaderKey(k)) {
				delete(h, k)
			}
		}

		n.ResponseWriter.WriteHeader(code)

		return
	}

	clear(h)

	n.swallow = true
	body, _ := json.Marshal(map[string]any{
		"type": "about:blank", "title": http.StatusText(code), "status": code, "code": "node_refused",
		"detail": "the node refused the connection",
	})

	h.Set("Content-Type", "application/problem+json")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
	n.ResponseWriter.WriteHeader(code)
	_, _ = n.ResponseWriter.Write(body)
}

func (n *nodeResponse) Write(b []byte) (int, error) {
	if !n.done {
		n.WriteHeader(http.StatusOK)
	}

	if n.swallow {
		return len(b), nil
	}

	return n.ResponseWriter.Write(b)
}

// recorder records the authz answer.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}

	if room := maxAuthzBody - r.body.Len(); room > 0 {
		r.body.Write(b[:min(len(b), room)])
	}

	return len(b), nil
}

// NodeUpstreams (http.reverse_proxy.upstreams.meshsdr_nodes) returns the
// node address resolved by the authz of the same request.
type NodeUpstreams struct{}

// CaddyModule implements caddy.Module.
func (NodeUpstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.reverse_proxy.upstreams.meshsdr_nodes", New: func() caddy.Module { return new(NodeUpstreams) }}
}

// GetUpstreams implements reverseproxy.UpstreamSource.
func (NodeUpstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	addr, _ := caddyhttp.GetVar(r.Context(), varUpstream).(string)
	if addr == "" {
		return nil, errors.New("gateway: no node upstream resolved")
	}

	return []*reverseproxy.Upstream{{Dial: addr}}, nil
}

// NodeTransport (http.reverse_proxy.transport.meshsdr_node) dials the node
// over mTLS with the gateway client certificate and the node's own checks
// (NodeTLS), per request: no Caddy reload on certificate rotation.
type NodeTransport struct {
	Binding     string         `json:"binding"`
	DialTimeout caddy.Duration `json:"dial_timeout,omitempty"`

	tr *http.Transport
}

// CaddyModule implements caddy.Module.
func (NodeTransport) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.reverse_proxy.transport.meshsdr_node", New: func() caddy.Module { return new(NodeTransport) }}
}

// Provision implements caddy.Provisioner.
func (t *NodeTransport) Provision(caddy.Context) error {
	b, err := lookup(t.Binding)
	if err != nil {
		return err
	}

	timeout := time.Duration(t.DialTimeout)
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	t.tr = &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			id, _ := caddyhttp.GetVar(ctx, varNode).(string)
			if id == "" {
				return nil, errors.New("gateway: no node id for the dial")
			}

			cfg, err := b.nodeTLS(ctx, id)
			if err != nil {
				return nil, err
			}

			d := tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: cfg}

			return d.DialContext(ctx, network, addr)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 10 * time.Second,
	}

	return nil
}

// RoundTrip implements http.RoundTripper.
func (t *NodeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "https"

	return t.tr.RoundTrip(req)
}

// Cleanup implements caddy.CleanerUpper.
func (t *NodeTransport) Cleanup() error {
	if t.tr != nil {
		t.tr.CloseIdleConnections()
	}

	return nil
}

// SlogWriter (caddy.logging.writers.slog) re-emits Caddy's JSON log entries
// on the injected logger, with component gateway.caddy.<logger>.
type SlogWriter struct {
	Binding string `json:"binding"`
}

// CaddyModule implements caddy.Module.
func (SlogWriter) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "caddy.logging.writers.slog", New: func() caddy.Module { return new(SlogWriter) }}
}

func (w SlogWriter) String() string { return "slog:" + w.Binding }

// WriterKey implements caddy.WriterOpener.
func (w SlogWriter) WriterKey() string { return "slog:" + w.Binding }

// OpenWriter implements caddy.WriterOpener.
func (w SlogWriter) OpenWriter() (io.WriteCloser, error) {
	b, err := lookup(w.Binding)
	if err != nil {
		return nil, err
	}

	return &slogSink{log: b.logger}, nil
}

type slogSink struct{ log *slog.Logger }

func (s *slogSink) Close() error { return nil }

func (s *slogSink) Write(p []byte) (int, error) {
	sc := bufio.NewScanner(bytes.NewReader(p))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for sc.Scan() {
		s.emit(sc.Bytes())
	}

	return len(p), nil
}

func (s *slogSink) emit(line []byte) {
	ctx := context.Background()

	var e map[string]any
	if err := json.Unmarshal(line, &e); err != nil {
		s.log.LogAttrs(ctx, slog.LevelInfo, strings.TrimSpace(string(line)), slog.String("component", "gateway.caddy.stdlib"))

		return
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

	if !s.log.Enabled(ctx, level) {
		return
	}

	msg, _ := e["msg"].(string)
	component := "gateway.caddy"

	if name, _ := e["logger"].(string); name != "" {
		component += "." + name
	}

	ts := time.Now()
	if f, ok := e["ts"].(float64); ok {
		ts = time.Unix(0, int64(f*1e9))
	}

	rec := slog.NewRecord(ts, level, msg, 0)
	rec.AddAttrs(slog.String("component", component))

	for k, v := range e {
		switch k {
		case "level", "msg", "logger", "ts":
		default:
			rec.AddAttrs(slog.Any(k, redact(k, v)))
		}
	}

	_ = s.log.Handler().Handle(ctx, rec)
}

// secretHeaders are never logged, whatever Caddy's log level: the access
// token and the browser's credentials.
var secretHeaders = []string{"x-rx-access-token", "cookie", "set-cookie", "authorization", "proxy-authorization"}

// redact replaces the values of secret headers anywhere in a decoded log
// entry (request.headers, headers, …).
func redact(key string, v any) any {
	if slices.Contains(secretHeaders, strings.ToLower(key)) {
		return "REDACTED"
	}

	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = redact(k, x)
		}

		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = redact("", x)
		}

		return out
	default:
		return v
	}
}

var (
	_ caddyhttp.MiddlewareHandler = (*HubHandler)(nil)
	_ caddyhttp.MiddlewareHandler = (*NodeAuthz)(nil)
	_ reverseproxy.UpstreamSource = (*NodeUpstreams)(nil)
	_ http.RoundTripper           = (*NodeTransport)(nil)
	_ caddy.CleanerUpper          = (*NodeTransport)(nil)
	_ caddy.WriterOpener          = (*SlogWriter)(nil)
)
