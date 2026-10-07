package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Server settings.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	// gracePeriod bounds the graceful shutdown; proxied WebSockets are
	// closed after it.
	gracePeriod = 10 * time.Second
	// defaultDialTimeout bounds the connection to a node.
	defaultDialTimeout = 3 * time.Second
)

// Gateway is the public front of the hub: one server on HTTPSListen (or
// HTTPListen with TLSOff), plus the redirect and ACME HTTP-01 server on
// HTTPListen when TLS is on.
type Gateway struct {
	o      Options
	public *url.URL
	tls    *tls.Config
	// acme is set with TLSACME.
	acme *autocert.Manager

	mu      sync.Mutex
	servers []*http.Server
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New checks the configuration and prepares the TLS settings. Operator
// certificate files are read here.
func New(o Options) (*Gateway, error) {
	if o.Hub == nil || o.NodeTLS == nil || o.Logger == nil {
		return nil, errors.New("gateway: hub, node TLS and logger are required")
	}

	pub, err := url.Parse(o.Config.PublicURL)
	if err != nil || pub.Hostname() == "" {
		return nil, fmt.Errorf("gateway: invalid public URL %q", o.Config.PublicURL)
	}

	if o.Config.DialTimeout <= 0 {
		o.Config.DialTimeout = defaultDialTimeout
	}

	g := &Gateway{o: o, public: pub}

	switch o.Config.TLSMode {
	case TLSOff:
		if o.Config.HTTPListen == "" {
			return nil, errors.New("gateway: tls_mode off needs http_listen")
		}
	case TLSFiles:
		cert, err := tls.LoadX509KeyPair(o.Config.CertFile, o.Config.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("gateway certificate: %w", err)
		}

		g.tls = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	case TLSInternal:
		if o.InternalCert == nil {
			return nil, errors.New("gateway: tls_mode internal needs the hub CA (tls.ca_cert)")
		}

		g.tls = &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return o.InternalCert()
			},
		}
	case TLSACME:
		if o.Config.StorageDir == "" {
			return nil, errors.New("gateway: tls_mode acme needs storage_dir")
		}

		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(o.Config.StorageDir),
			HostPolicy: autocert.HostWhitelist(pub.Hostname()),
			Email:      o.Config.ACMEEmail,
		}
		if o.Config.ACMECA != "" {
			m.Client = &acme.Client{DirectoryURL: o.Config.ACMECA}
		}

		g.acme = m
		g.tls = m.TLSConfig()
		g.tls.MinVersion = tls.VersionTLS12
	default:
		return nil, fmt.Errorf("gateway: unknown TLS mode %q", o.Config.TLSMode)
	}

	return g, nil
}

// Handler is the public handler: the node route, 404 on /nodes/* and
// /internal/*, the hub router for everything else.
func (g *Gateway) Handler() http.Handler { return http.HandlerFunc(g.serveHTTP) }

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if m := nodePath.FindStringSubmatch(r.URL.Path); m != nil {
		g.serveNode(w, r, m[1])

		return
	}

	if reserved(r.URL.Path) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)

		return
	}

	g.o.Hub.ServeHTTP(w, r)
}

// reserved reports whether p is under /nodes/ or /internal/, whatever its
// case and dot segments: only the node route is served there.
func reserved(p string) bool {
	p = strings.ToLower(p)

	for _, q := range []string{p, path.Clean("/" + p)} {
		if strings.HasPrefix(q, "/nodes/") || strings.HasPrefix(q, "/internal/") {
			return true
		}
	}

	return false
}

// redirect sends plain HTTP requests to the public URL.
func (g *Gateway) redirect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Location", g.public.Scheme+"://"+g.public.Host+r.URL.RequestURI())
	w.WriteHeader(http.StatusPermanentRedirect)
}

// Start opens the listeners and serves in the background: a listen error
// is returned, a later serve error is logged. ACME certificates are
// obtained on the first handshake.
func (g *Gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.servers != nil {
		return errors.New("gateway: already started")
	}

	if g.acme != nil {
		if err := os.MkdirAll(g.o.Config.StorageDir, 0o700); err != nil {
			return fmt.Errorf("gateway storage %s: %w", g.o.Config.StorageDir, err)
		}
	}

	// Requests (and the proxied WebSockets) live in base, cancelled by
	// Stop after the grace period.
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))

	type bound struct {
		srv *http.Server
		ln  net.Listener
	}

	var bounds []bound

	open := func(addr string, h http.Handler, tlsConf *tls.Config) error {
		var lc net.ListenConfig

		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("gateway listen on %s: %w", addr, err)
		}

		var protocols http.Protocols

		protocols.SetHTTP1(true)
		protocols.SetHTTP2(tlsConf != nil)

		srv := &http.Server{
			Handler:           h,
			TLSConfig:         tlsConf,
			Protocols:         &protocols,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
			// TLS handshake failures and the like: diagnosis only.
			ErrorLog:    slog.NewLogLogger(g.o.Logger.Handler(), slog.LevelDebug),
			BaseContext: func(net.Listener) context.Context { return base },
		}
		bounds = append(bounds, bound{srv: srv, ln: ln})

		return nil
	}

	var err error

	if g.tls == nil {
		err = open(g.o.Config.HTTPListen, g.Handler(), nil)
	} else {
		err = open(g.o.Config.HTTPSListen, g.Handler(), g.tls)
		if err == nil && g.o.Config.HTTPListen != "" {
			var plain http.Handler = http.HandlerFunc(g.redirect)
			if g.acme != nil {
				plain = g.acme.HTTPHandler(plain)
			}

			err = open(g.o.Config.HTTPListen, plain, nil)
		}
	}

	if err != nil {
		for _, b := range bounds {
			_ = b.ln.Close()
		}

		cancel()

		return err
	}

	g.cancel = cancel

	for _, b := range bounds {
		g.servers = append(g.servers, b.srv)
		g.wg.Go(func() { g.serve(base, b.srv, b.ln) })
	}

	g.o.Logger.InfoContext(ctx, "gateway started", slog.String("tls_mode", g.o.Config.TLSMode),
		slog.String("https_listen", g.o.Config.HTTPSListen), slog.String("http_listen", g.o.Config.HTTPListen))

	return nil
}

func (g *Gateway) serve(ctx context.Context, srv *http.Server, ln net.Listener) {
	var err error

	addr := ln.Addr().String()

	g.o.Logger.DebugContext(ctx, "gateway listening", slog.String("addr", addr), slog.Bool("tls", srv.TLSConfig != nil))

	if srv.TLSConfig != nil {
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
	}

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		g.o.Logger.ErrorContext(ctx, "gateway server failed", slog.String("addr", addr), slog.Any("error", err))
	}
}

// Stop closes the listeners, waits up to the grace period for requests in
// flight, then closes the remaining connections, proxied WebSockets
// included.
func (g *Gateway) Stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.servers == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), gracePeriod)
	defer cancel()

	var errs []error

	for _, srv := range g.servers {
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()

			if !errors.Is(err, context.DeadlineExceeded) {
				errs = append(errs, err)
			}
		}
	}

	// Hijacked connections are not tracked by Shutdown: cancelling the
	// base context closes the proxied WebSockets.
	g.cancel()
	g.wg.Wait()
	g.servers = nil

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("stop gateway: %w", err)
	}

	return nil
}
