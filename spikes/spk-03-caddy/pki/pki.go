// Package pki is a throwaway stand-in for the hub internal CA (TECHNICAL_SPEC §4.1).
// It issues: the gateway client cert (URI SAN urn:rx:gateway:<hub>), node server
// certs (URI SAN urn:rx:node:<id> + DNS <id>.nodes.rx.internal + 127.0.0.1) and a
// self-signed "operator-provided" public cert for the browser-facing listener.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// NodeDNSSuffix is the private DNS suffix used as TLS server name toward nodes.
const NodeDNSSuffix = ".nodes.rx.internal"

type CA struct {
	Dir  string
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	Pool *x509.CertPool
}

// Paths of the PEM files written for one leaf.
type Leaf struct {
	CertFile, KeyFile string
	TLS               tls.Certificate
}

func NewCA(dir string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "MeshSDR spike hub CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(30 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	ca := &CA{Dir: dir, cert: cert, key: key, Pool: pool}
	return ca, writePEM(filepath.Join(dir, "hub-ca.pem"), "CERTIFICATE", der)
}

func (ca *CA) CAFile() string { return filepath.Join(ca.Dir, "hub-ca.pem") }

var serial int64 = 1

func (ca *CA) issue(name string, tpl *x509.Certificate, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (Leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Leaf{}, err
	}
	serial++
	tpl.SerialNumber = big.NewInt(serial)
	tpl.NotBefore = time.Now().Add(-time.Hour)
	tpl.NotAfter = time.Now().Add(30 * 24 * time.Hour)
	tpl.KeyUsage = x509.KeyUsageDigitalSignature
	if parent == nil { // self-signed
		parent, parentKey = tpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		return Leaf{}, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Leaf{}, err
	}
	l := Leaf{CertFile: filepath.Join(ca.Dir, name+".crt"), KeyFile: filepath.Join(ca.Dir, name+".key")}
	if err := writePEM(l.CertFile, "CERTIFICATE", der); err != nil {
		return Leaf{}, err
	}
	if err := writePEM(l.KeyFile, "EC PRIVATE KEY", kder); err != nil {
		return Leaf{}, err
	}
	l.TLS, err = tls.LoadX509KeyPair(l.CertFile, l.KeyFile)
	return l, err
}

// Gateway issues the gateway client certificate (URI SAN urn:rx:gateway:<hubID>).
func (ca *CA) Gateway(hubID string) (Leaf, error) {
	u, _ := url.Parse("urn:rx:gateway:" + hubID)
	return ca.issue("gateway", &x509.Certificate{
		Subject:     pkix.Name{CommonName: "gateway"},
		URIs:        []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca.cert, ca.key)
}

// Node issues a node server certificate.
func (ca *CA) Node(id string) (Leaf, error) {
	u, _ := url.Parse("urn:rx:node:" + id)
	return ca.issue("node-"+id, &x509.Certificate{
		Subject:     pkix.Name{CommonName: id},
		URIs:        []*url.URL{u},
		DNSNames:    []string{id + NodeDNSSuffix},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca.cert, ca.key)
}

// OperatorCert writes a self-signed public certificate, standing in for an
// operator-provided cert/key pair (INT-001 "configured cert/key paths").
func (ca *CA) OperatorCert(host string) (Leaf, error) {
	return ca.issue("operator", &x509.Certificate{
		Subject:     pkix.Name{CommonName: host},
		DNSNames:    []string{host},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, nil, nil)
}

func writePEM(path, typ string, der []byte) error {
	b := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
