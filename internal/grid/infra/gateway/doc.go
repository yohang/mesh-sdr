// Package gateway is the hub gateway (ADR 0002, ADR 0012, ADR 0021): the
// only public listener of the hub, built on net/http. It terminates TLS
// (ACME, operator files, the hub CA or off), serves the hub router
// in-process and proxies /nodes/{nodeId}/ws to the node over mTLS:
//
//  1. delete every client-supplied X-Rx-* header;
//  2. forward auth, in-process: the hub router answers a sub-request to
//     AuthzPath; a non-2xx answer goes back to the browser as is, a 2xx one
//     gives the access token, the cid and the node address
//     (HeaderUpstream);
//  3. reverse proxy to that address at /ws with the gateway client
//     certificate (NodeTLS), no buffering, keeping only an allow-list of
//     headers each way.
//
// Node enrollment, removal and address changes never touch the gateway:
// the upstream is resolved per request by the hub authz.
package gateway

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"time"
)

// AuthzPath is the hub route answering the forward-auth sub-request of a
// node media upgrade, with the node id in the "node" query parameter. The
// gateway answers 404 on it from outside.
const AuthzPath = "/internal/gateway/authz"

// Headers exchanged with the hub authz (TECHNICAL_SPEC §4.6, §5.16).
const (
	HeaderAccessToken = "X-Rx-Access-Token"
	HeaderCID         = "X-Rx-Cid"
	HeaderNodeID      = "X-Rx-Node-Id"
	// HeaderUpstream is set by the authz on success: host:port of the node.
	// It never leaves the hub.
	HeaderUpstream = "X-Rx-Upstream"
)

// TLS modes of the public listener.
const (
	TLSACME     = "acme"
	TLSFiles    = "files"
	TLSInternal = "internal"
	TLSOff      = "off"
)

// Config is the gateway configuration.
type Config struct {
	// HTTPSListen is the TLS listener (unused with TLSOff).
	HTTPSListen string
	// HTTPListen is the plain listener: the hub with TLSOff, otherwise a
	// redirect to PublicURL and ACME HTTP-01 challenges. Empty: none.
	HTTPListen string
	TLSMode    string
	// CertFile and KeyFile are the operator certificate (TLSFiles).
	CertFile, KeyFile string
	// PublicURL is hub.url: its host is certified (TLSACME, TLSInternal)
	// and redirects go to it.
	PublicURL string
	ACMEEmail string
	// ACMECA is the ACME directory URL; empty: Let's Encrypt production.
	ACMECA string
	// StorageDir holds the ACME account and certificates (TLSACME).
	StorageDir string
	// StreamTimeout bounds the lifetime of a proxied node connection; zero
	// means no bound.
	StreamTimeout time.Duration
	// DialTimeout bounds the connection to a node (default 3 s).
	DialTimeout time.Duration
	// MaxBody caps the request body of the node route (gateway.max_body);
	// the hub router caps its own.
	MaxBody int64
}

// Options are the dependencies of a Gateway.
type Options struct {
	Config Config
	// Hub is the hub router, served in-process; it also answers AuthzPath.
	Hub http.Handler
	// NodeTLS returns the client TLS config to dial node nodeID: the
	// gateway certificate, the node server name and its pinned
	// certificate.
	NodeTLS func(ctx context.Context, nodeID string) (*tls.Config, error)
	// InternalCert returns the public certificate for TLSInternal, issued
	// by the hub CA for the host of PublicURL.
	InternalCert func() (*tls.Certificate, error)
	Logger       *slog.Logger
}
