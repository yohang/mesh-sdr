// Package gateway embeds Caddy as the hub gateway (ADR 0002, option C; ADR
// 0012): the only public listener of the hub. It terminates TLS (ACME,
// operator files, internal CA or off), serves the hub router in-process and
// proxies /nodes/{nodeId}/ws to the node over mTLS through one static route:
//
//  1. delete every client-supplied X-Rx-* header;
//  2. forward auth, in-process: the hub router answers a sub-request to
//     AuthzPath; a non-2xx answer goes back to the browser as is, a 2xx one
//     sets X-Rx-Access-Token and X-Rx-Cid on the request and names the node
//     address (HeaderUpstream);
//  3. rewrite the path to /ws;
//  4. reverse proxy to that address with the gateway client certificate
//     (NodeTLS), no buffering.
//
// Node enrollment, removal and address changes never touch the Caddy
// config: the upstream is resolved per request by the hub authz.
//
// Caddy builds its modules from JSON and cannot receive constructor
// dependencies, so the custom modules find theirs in a package-level
// binding table filled by New: the documented exception to the "no globals"
// rule (ADR 0002 decision 5), confined to this package. Caddy is a process
// singleton: one Gateway runs at a time.
//
// Builds with the nogateway tag leave Caddy out (node-only binary): New then
// returns ErrUnavailable.
package gateway

import (
	"context"
	"crypto/tls"
	"errors"
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

// ErrUnavailable is returned by New in a nogateway build.
var ErrUnavailable = errors.New("this meshsdr binary was built without the gateway (-tags nogateway): " +
	"it can only run the node role; use the full binary for the hub and all roles")

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
	ACMECA    string
	// StorageDir holds the ACME account, managed certificates and the
	// internal CA.
	StorageDir       string
	StreamCloseDelay time.Duration
	StreamTimeout    time.Duration
	DialTimeout      time.Duration
	// LogLevel is log.level (debug, info, warn, error).
	LogLevel string
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
	Logger  *slog.Logger
}
