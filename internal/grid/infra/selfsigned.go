// Package infra holds the grid adapters.
package infra

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

// selfSignedValidity is long enough for a node that waits for enrollment;
// the certificate only lives in memory and is regenerated at every start.
const selfSignedValidity = 365 * 24 * time.Hour

// SelfSignedCertificate returns an ephemeral, in-memory ECDSA P-256
// certificate for a node that is not enrolled yet (TECHNICAL_SPEC §4.2 step
// 3). Its SAN holds the URI urn:rx:node:<id> and, when listen names a
// specific host, that IP or DNS name. Nothing is written to disk.
func SelfSignedCertificate(id domain.NodeID, listen string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate node key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: id.String()},
		URIs:                  []*url.URL{{Scheme: "urn", Opaque: "rx:node:" + id.String()}},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	if host, _, err := net.SplitHostPort(listen); err == nil && host != "" {
		if ip := net.ParseIP(host); ip != nil {
			if !ip.IsUnspecified() {
				tmpl.IPAddresses = []net.IP{ip}
			}
		} else {
			tmpl.DNSNames = []string{host}
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create self-signed certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse self-signed certificate: %w", err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
