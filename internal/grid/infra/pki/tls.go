package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// CertSource mints and caches an in-memory certificate: a client
// certificate (hub or gateway) or the public server certificate of
// gateway.tls_mode = internal. It mints a new one when 2/3 of its validity
// has elapsed.
type CertSource struct {
	mint func(now time.Time) (tls.Certificate, error)
	now  func() time.Time

	mu   sync.Mutex
	cert *tls.Certificate
}

// NewClientSource returns a source of urn:rx:<kind>:<id> client certificates.
func NewClientSource(ca *CA, kind, id string, now func() time.Time) *CertSource {
	return &CertSource{mint: func(t time.Time) (tls.Certificate, error) { return ca.MintClient(kind, id, t) }, now: now}
}

// NewServerSource returns a source of server certificates for host (a DNS
// name or an IP address).
func NewServerSource(ca *CA, host string, now func() time.Time) *CertSource {
	return &CertSource{mint: func(t time.Time) (tls.Certificate, error) { return ca.MintServer(host, t) }, now: now}
}

// Get returns the current certificate.
func (s *CertSource) Get() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.cert == nil || RenewalDue(s.cert.Leaf, now) {
		c, err := s.mint(now)
		if err != nil {
			return nil, err
		}

		s.cert = &c
	}

	return s.cert, nil
}

// LeafCheck is an extra check of a verified peer leaf (pinning, revocation).
type LeafCheck func(leaf *x509.Certificate) error

// HubDialConfig is the TLS config of a hub connection to node nodeID: TLS
// 1.3, the node's server name, chain to the CA, the node URI SAN, then
// check. The client certificate comes from client.
func HubDialConfig(client *CertSource, roots *x509.CertPool, nodeID string, check LeafCheck) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots,
		ServerName: NodeServerName(nodeID),
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return client.Get()
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("%w: no node certificate", ErrInvalidCertificate)
			}

			leaf := cs.PeerCertificates[0]
			if kind, id, ok := Identity(leaf); !ok || kind != KindNode || id != nodeID {
				return fmt.Errorf("%w: peer is not %s", ErrInvalidCertificate, IdentityURI(KindNode, nodeID))
			}

			if check != nil {
				return check(leaf)
			}

			return nil
		},
	}
}

// CertHolder holds a hot-swappable server certificate.
type CertHolder struct {
	mu   sync.RWMutex
	cert *tls.Certificate
}

// NewCertHolder returns a holder of cert.
func NewCertHolder(cert tls.Certificate) *CertHolder { return &CertHolder{cert: &cert} }

// Get returns the current certificate.
func (h *CertHolder) Get() *tls.Certificate {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.cert
}

// Serial returns the serial of the current leaf ("" when unknown).
func (h *CertHolder) Serial() string {
	c := h.Get()
	if c == nil || c.Leaf == nil {
		return ""
	}

	return SerialString(c.Leaf)
}

// Set replaces the certificate.
func (h *CertHolder) Set(cert tls.Certificate) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.cert = &cert
}

// RevokedSet is the set of revoked certificate serials pushed by the hub.
type RevokedSet struct {
	mu      sync.RWMutex
	serials map[string]struct{}
}

// NewRevokedSet returns an empty set.
func NewRevokedSet() *RevokedSet { return &RevokedSet{serials: map[string]struct{}{}} }

// Add records serials.
func (s *RevokedSet) Add(serials ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, v := range serials {
		s.serials[v] = struct{}{}
	}
}

// Contains reports whether serial is revoked.
func (s *RevokedSet) Contains(serial string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.serials[serial]

	return ok
}

// ErrRevoked is returned for a revoked peer certificate.
var ErrRevoked = errors.New("certificate_revoked")

// NodeServerConfig is the TLS config of an enrolled node: TLS 1.3, a client
// certificate chaining to the hub CA is required, revoked serials are
// refused. Which identity may use which path is checked by the handlers
// (PeerIdentity).
func NodeServerConfig(cert *CertHolder, roots *x509.CertPool, revoked *RevokedSet) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  roots,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return cert.Get(), nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("%w: client certificate required", ErrInvalidCertificate)
			}

			leaf := cs.PeerCertificates[0]
			if revoked != nil && revoked.Contains(SerialString(leaf)) {
				return ErrRevoked
			}

			if _, _, ok := Identity(leaf); !ok {
				return fmt.Errorf("%w: no rx identity", ErrInvalidCertificate)
			}

			return nil
		},
	}
}

// PeerIdentity returns the rx identity of the verified client certificate
// of r.
func PeerIdentity(r *http.Request) (kind, id string, ok bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", "", false
	}

	return Identity(r.TLS.PeerCertificates[0])
}
