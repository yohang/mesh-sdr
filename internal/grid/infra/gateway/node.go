package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// nodePath matches the node media route (node id slug, §4.1).
var nodePath = regexp.MustCompile(`^/nodes/([a-z0-9][a-z0-9-]{1,62})/ws$`)

// responseHeaderTimeout bounds the wait for the node's answer.
const responseHeaderTimeout = 10 * time.Second

// maxAuthzBody bounds the recorded authz answer.
const maxAuthzBody = 64 << 10

// forwardedRequestHeaders are the browser headers a node receives (§5.8):
// the WebSocket handshake, Origin and User-Agent. Cookie, Authorization
// and every other header are dropped.
var forwardedRequestHeaders = []string{
	"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Protocol",
	"Sec-Websocket-Extensions", "Origin", "User-Agent",
}

// upgradeResponseHeaders are the node response headers the browser
// receives on a successful upgrade.
var upgradeResponseHeaders = []string{
	"Connection", "Upgrade", "Sec-Websocket-Accept", "Sec-Websocket-Protocol", "Sec-Websocket-Extensions",
}

// grant is a successful authz answer.
type grant struct {
	node, upstream, token, cid string
}

// serveNode is the /nodes/{id}/ws route: strip X-Rx-*, authorize through
// the hub, proxy to the node.
func (g *Gateway) serveNode(w http.ResponseWriter, r *http.Request, id string) {
	for name := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Rx-") {
			delete(r.Header, name)
		}
	}

	gr, ok := g.authorize(w, r, id)
	if !ok {
		return
	}

	cfg, err := g.o.NodeTLS(r.Context(), id)
	if err != nil {
		g.o.Logger.LogAttrs(r.Context(), slog.LevelWarn, "node media connection failed",
			slog.String("node_id", id), slog.Any("error", err))
		badGateway(w)

		return
	}

	ctx := r.Context()

	if t := g.o.Config.StreamTimeout; t > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}

	r = r.WithContext(ctx)

	if g.o.Config.MaxBody > 0 && r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, g.o.Config.MaxBody)
	}

	tr := g.nodeTransport(gr.upstream, cfg)
	defer tr.CloseIdleConnections()

	proxy := &httputil.ReverseProxy{
		Rewrite:        func(pr *httputil.ProxyRequest) { rewrite(pr, gr) },
		Transport:      tr,
		FlushInterval:  -1,
		ModifyResponse: g.filterResponse(gr),
		ErrorHandler:   g.proxyError(gr),
		ErrorLog:       slog.NewLogLogger(g.o.Logger.Handler(), slog.LevelDebug),
	}

	proxy.ServeHTTP(&nodeWriter{ResponseWriter: w}, r)
}

// authorize runs the hub authz in-process. On refusal it writes the hub
// answer and returns false.
func (g *Gateway) authorize(w http.ResponseWriter, r *http.Request, id string) (grant, bool) {
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
	g.o.Hub.ServeHTTP(rec, sub)

	if rec.status == 0 {
		rec.status = http.StatusOK
	}

	if rec.status/100 != 2 {
		for k, v := range rec.header {
			if !strings.HasPrefix(k, "X-Rx-") {
				w.Header()[k] = v
			}
		}

		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())

		return grant{}, false
	}

	gr := grant{
		node: id, upstream: rec.header.Get(HeaderUpstream),
		token: rec.header.Get(HeaderAccessToken), cid: rec.header.Get(HeaderCID),
	}

	if gr.upstream == "" {
		g.o.Logger.LogAttrs(r.Context(), slog.LevelError, "authz answered without an upstream",
			slog.String("node_id", id))
		badGateway(w)

		return grant{}, false
	}

	return gr, true
}

// rewrite targets the node at /ws (the query is kept) and keeps only the
// allowed headers, plus the token, cid and node id set by the authz.
func rewrite(pr *httputil.ProxyRequest, gr grant) {
	pr.Out.URL = &url.URL{Scheme: "https", Host: gr.upstream, Path: "/ws", RawQuery: pr.In.URL.RawQuery}
	pr.Out.Host = pr.In.Host

	h := http.Header{}

	for _, k := range forwardedRequestHeaders {
		if v, ok := pr.Out.Header[k]; ok {
			h[k] = v
		}
	}

	h.Set(HeaderAccessToken, gr.token)
	h.Set(HeaderCID, gr.cid)
	h.Set(HeaderNodeID, gr.node)
	pr.Out.Header = h
}

// nodeTransport dials upstream over mTLS with cfg, one connection per
// request: the gateway certificate, the node server name, pin and
// revocation checks all come from cfg (NodeTLS).
func (g *Gateway) nodeTransport(upstream string, cfg *tls.Config) *http.Transport {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: g.o.Config.DialTimeout}, Config: cfg}

	return &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, upstream)
		},
		DisableKeepAlives:     true,
		DisableCompression:    true,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// filterResponse keeps the handshake headers of a successful upgrade and
// replaces any other node answer by a hub problem with the same status, so
// no node content (cookies, scripts, documents) is served on the hub
// origin.
func (g *Gateway) filterResponse(gr grant) func(*http.Response) error {
	return func(resp *http.Response) error {
		ctx := resp.Request.Context()

		if resp.StatusCode == http.StatusSwitchingProtocols {
			for k := range resp.Header {
				if !slices.Contains(upgradeResponseHeaders, http.CanonicalHeaderKey(k)) {
					delete(resp.Header, k)
				}
			}

			g.o.Logger.LogAttrs(ctx, slog.LevelDebug, "node media connection upgraded",
				slog.String("node_id", gr.node), slog.String("cid", gr.cid))

			return nil
		}

		g.o.Logger.LogAttrs(ctx, slog.LevelDebug, "node refused the media connection",
			slog.String("node_id", gr.node), slog.Int("status", resp.StatusCode))

		_ = resp.Body.Close()

		body, _ := json.Marshal(map[string]any{
			"type": "about:blank", "title": http.StatusText(resp.StatusCode), "status": resp.StatusCode,
			"code": "node_refused", "detail": "the node refused the connection",
		})

		resp.Header = http.Header{}
		hardened(resp.Header)
		resp.Header.Set("Content-Type", "application/problem+json")
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.TransferEncoding = nil
		resp.Trailer = nil

		return nil
	}
}

// proxyError answers 502 when the node cannot be reached.
func (g *Gateway) proxyError(gr grant) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		level := slog.LevelWarn
		if errors.Is(err, context.Canceled) {
			level = slog.LevelDebug
		}

		g.o.Logger.LogAttrs(r.Context(), level, "node media connection failed",
			slog.String("node_id", gr.node), slog.Any("error", err))
		badGateway(w)
	}
}

// hardened sets the headers of every gateway-made answer on the node route.
func hardened(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
}

func badGateway(w http.ResponseWriter) {
	hardened(w.Header())
	w.WriteHeader(http.StatusBadGateway)
}

// nodeWriter drops the informational answers of a node (1xx other than
// 101), which the reverse proxy would otherwise relay with their headers.
type nodeWriter struct {
	http.ResponseWriter
}

// Unwrap lets http.ResponseController reach Hijack and Flush.
func (n *nodeWriter) Unwrap() http.ResponseWriter { return n.ResponseWriter }

func (n *nodeWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}

	n.ResponseWriter.WriteHeader(code)
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
