// Package pki is the hub internal CA and the certificates of the hub ↔ node
// links (TECHNICAL_SPEC §4.1, §4.3, ADR 0008): ECDSA P-256 keys, a 10-year
// CA, 90-day node leaves renewed at 2/3 of their validity, and an in-memory
// hub client certificate minted from the CA key.
//
// Identities are URI SANs: urn:rx:node:<id>, urn:rx:hub:<hub-id> and
// urn:rx:gateway:<hub-id>. Node leaves also carry the DNS SAN
// <id>.nodes.rx.internal, which the hub and the gateway use as server name.
package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Validity periods.
const (
	CAValidity   = 10 * 365 * 24 * time.Hour
	NodeValidity = 90 * 24 * time.Hour
	HubValidity  = 30 * 24 * time.Hour
	// clockSkew backdates NotBefore to tolerate small clock differences.
	clockSkew = 5 * time.Minute
)

// NodeDNSSuffix is the DNS suffix of node server names.
const NodeDNSSuffix = ".nodes.rx.internal"

// Identity kinds carried in URI SANs.
const (
	KindNode    = "node"
	KindHub     = "hub"
	KindGateway = "gateway"
)

// ErrInvalidCertificate is returned when a certificate fails a check.
var ErrInvalidCertificate = errors.New("invalid_certificate")

// NodeServerName is the TLS server name of node id.
func NodeServerName(id string) string { return id + NodeDNSSuffix }

// IdentityURI returns urn:rx:<kind>:<id>.
func IdentityURI(kind, id string) *url.URL {
	return &url.URL{Scheme: "urn", Opaque: "rx:" + kind + ":" + id}
}

// Identity returns the first rx identity (kind, id) in cert's URI SANs.
func Identity(cert *x509.Certificate) (kind, id string, ok bool) {
	for _, u := range cert.URIs {
		if u.Scheme != "urn" {
			continue
		}

		rest, found := strings.CutPrefix(u.Opaque, "rx:")
		if !found {
			continue
		}

		kind, id, ok = strings.Cut(rest, ":")
		if ok && kind != "" && id != "" {
			return kind, id, true
		}
	}

	return "", "", false
}

// Fingerprint is the SHA-256 of a DER certificate.
func Fingerprint(der []byte) [32]byte { return sha256.Sum256(der) }

// FormatFingerprint renders a fingerprint as colon-separated upper-case hex.
func FormatFingerprint(fp [32]byte) string {
	parts := make([]string, len(fp))
	for i, b := range fp {
		parts[i] = fmt.Sprintf("%02X", b)
	}

	return strings.Join(parts, ":")
}

// ParseFingerprint reads a SHA-256 fingerprint in hex, with or without
// colons, in any case.
func ParseFingerprint(s string) ([32]byte, error) {
	var fp [32]byte

	b, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
	if err != nil || len(b) != len(fp) {
		return fp, errors.New("fingerprint must be 32 bytes of hex (SHA-256)")
	}

	copy(fp[:], b)

	return fp, nil
}

// SerialString renders a certificate serial as upper-case hex.
func SerialString(cert *x509.Certificate) string { return strings.ToUpper(cert.SerialNumber.Text(16)) }

// GenerateKey returns a new ECDSA P-256 key.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	return k, nil
}

func newSerial() (*big.Int, error) {
	s, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	return s.Add(s, big.NewInt(1)), nil
}

// listenSANs returns the IP or DNS SAN of a specific listen host.
func listenSANs(listen string) ([]net.IP, []string) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return nil, nil
	}

	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() {
			return nil, nil
		}

		return []net.IP{ip}, nil
	}

	return nil, []string{host}
}

// GenerateCA returns a new self-signed CA certificate and its key, PEM
// encoded.
func GenerateCA(commonName string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := GenerateKey()
	if err != nil {
		return nil, nil, err
	}

	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"MeshSDR"}},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}

	keyPEM, err = EncodeKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}

	return EncodeCertsPEM(der), keyPEM, nil
}

// CA is the hub internal certificate authority.
type CA struct {
	cert *x509.Certificate
	key  crypto.Signer
	pool *x509.CertPool
}

// ParseCA loads a CA from its PEM certificate and key, checking that they
// match and that the certificate is a CA.
func ParseCA(certPEM, keyPEM []byte) (*CA, error) {
	cert, err := ParseCACert(certPEM)
	if err != nil {
		return nil, err
	}

	key, err := ParseKeyPEM(keyPEM)
	if err != nil {
		return nil, err
	}

	if !publicKeysEqual(cert.PublicKey, key.Public()) {
		return nil, fmt.Errorf("%w: CA key does not match the CA certificate", ErrInvalidCertificate)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return &CA{cert: cert, key: key, pool: pool}, nil
}

// ParseCACert reads a PEM CA certificate.
func ParseCACert(certPEM []byte) (*x509.Certificate, error) {
	certs, err := ParseCertsPEM(certPEM)
	if err != nil {
		return nil, err
	}

	if len(certs) != 1 {
		return nil, fmt.Errorf("%w: want exactly one CA certificate, got %d", ErrInvalidCertificate, len(certs))
	}

	if !certs[0].IsCA {
		return nil, fmt.Errorf("%w: certificate is not a CA", ErrInvalidCertificate)
	}

	return certs[0], nil
}

// Certificate returns the CA certificate.
func (ca *CA) Certificate() *x509.Certificate { return ca.cert }

// Pool returns a pool holding the CA certificate.
func (ca *CA) Pool() *x509.CertPool { return ca.pool }

// Fingerprint returns the SHA-256 of the CA certificate.
func (ca *CA) Fingerprint() [32]byte { return Fingerprint(ca.cert.Raw) }

// PEM returns the CA certificate in PEM.
func (ca *CA) PEM() []byte { return EncodeCertsPEM(ca.cert.Raw) }

// SignNodeCSR verifies a node CSR and issues its leaf certificate (DER).
// The CSR must be self-consistent and carry urn:rx:node:<nodeID>. The leaf
// SANs are fixed by the hub, never taken from the CSR: the node URI, the
// DNS name <nodeID>.nodes.rx.internal and host, the host of the node URL
// the hub dials (an IP or a DNS name).
func (ca *CA) SignNodeCSR(csrDER []byte, nodeID, host string, now time.Time) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("%w: parse CSR: %w", ErrInvalidCertificate, err)
	}

	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: CSR signature: %w", ErrInvalidCertificate, err)
	}

	kind, id, ok := identityOfURIs(csr.URIs)
	if !ok || kind != KindNode || id != nodeID {
		return nil, fmt.Errorf("%w: CSR identity is not %s", ErrInvalidCertificate, IdentityURI(KindNode, nodeID))
	}

	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, fmt.Errorf("%w: CSR key must be ECDSA P-256", ErrInvalidCertificate)
	}

	dns := []string{NodeServerName(nodeID)}

	var ips []net.IP

	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if host != "" && host != dns[0] {
		dns = append(dns, host)
	}

	return ca.issueNode(csr.PublicKey, nodeID, ips, dns, now)
}

// RenewNode issues a new leaf for the key and SANs of an existing node leaf.
func (ca *CA) RenewNode(leaf *x509.Certificate, now time.Time) ([]byte, error) {
	kind, id, ok := Identity(leaf)
	if !ok || kind != KindNode {
		return nil, fmt.Errorf("%w: not a node certificate", ErrInvalidCertificate)
	}

	return ca.issueNode(leaf.PublicKey, id, leaf.IPAddresses, leaf.DNSNames, now)
}

func (ca *CA) issueNode(pub any, nodeID string, ips []net.IP, dns []string, now time.Time) ([]byte, error) {
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID},
		URIs:         []*url.URL{IdentityURI(KindNode, nodeID)},
		DNSNames:     dns,
		IPAddresses:  ips,
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(NodeValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		return nil, fmt.Errorf("issue node certificate: %w", err)
	}

	return der, nil
}

// MintClient issues an in-memory client certificate with a fresh key for
// urn:rx:<kind>:<id> (the hub and the gateway).
func (ca *CA) MintClient(kind, id string, now time.Time) (tls.Certificate, error) {
	key, err := GenerateKey()
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, err := newSerial()
	if err != nil {
		return tls.Certificate{}, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: kind + ":" + id},
		URIs:         []*url.URL{IdentityURI(kind, id)},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(HubValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("issue %s certificate: %w", kind, err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse %s certificate: %w", kind, err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// CreateNodeCSR returns a DER CSR for key with urn:rx:node:<nodeID> and the
// SANs of a specific listen host.
func CreateNodeCSR(key *ecdsa.PrivateKey, nodeID, listen string) ([]byte, error) {
	ips, dns := listenSANs(listen)

	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: nodeID},
		URIs:        []*url.URL{IdentityURI(KindNode, nodeID)},
		IPAddresses: ips,
		DNSNames:    dns,
	}, key)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}

	return der, nil
}

// SelfSigned returns a self-signed certificate of key for a node that is
// not enrolled yet (§4.2 step 3).
func SelfSigned(key *ecdsa.PrivateKey, nodeID, listen string, now time.Time) (tls.Certificate, error) {
	serial, err := newSerial()
	if err != nil {
		return tls.Certificate{}, err
	}

	ips, dns := listenSANs(listen)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: nodeID},
		URIs:                  []*url.URL{IdentityURI(KindNode, nodeID)},
		IPAddresses:           ips,
		DNSNames:              dns,
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
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

// VerifyLeaf checks that leaf chains to roots for usage at now.
func VerifyLeaf(leaf *x509.Certificate, roots *x509.CertPool, usage x509.ExtKeyUsage, now time.Time) error {
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCertificate, err)
	}

	return nil
}

// RenewalDue reports whether 2/3 of leaf's validity has elapsed at now.
func RenewalDue(leaf *x509.Certificate, now time.Time) bool {
	life := leaf.NotAfter.Sub(leaf.NotBefore)

	return !now.Before(leaf.NotBefore.Add(life * 2 / 3))
}

func identityOfURIs(uris []*url.URL) (kind, id string, ok bool) {
	return Identity(&x509.Certificate{URIs: uris})
}

func publicKeysEqual(a, b any) bool {
	ka, ok := a.(interface{ Equal(crypto.PublicKey) bool })

	return ok && ka.Equal(b)
}

// EncodeCertsPEM encodes DER certificates as PEM blocks.
func EncodeCertsPEM(ders ...[]byte) []byte {
	var buf bytes.Buffer
	for _, der := range ders {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}

	return buf.Bytes()
}

// EncodeKeyPEM encodes a private key as a PKCS #8 PEM block.
func EncodeKeyPEM(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseCertsPEM reads every CERTIFICATE block of data.
func ParseCertsPEM(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate

	for {
		var block *pem.Block

		block, data = pem.Decode(data)
		if block == nil {
			break
		}

		if block.Type != "CERTIFICATE" {
			continue
		}

		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCertificate, err)
		}

		certs = append(certs, c)
	}

	if len(certs) == 0 {
		return nil, fmt.Errorf("%w: no PEM certificate found", ErrInvalidCertificate)
	}

	return certs, nil
}

// ParseKeyPEM reads a PKCS #8, SEC 1 (EC) private key PEM block.
func ParseKeyPEM(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM private key found")
	}

	if block.Type == "EC PRIVATE KEY" {
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse key: %w", err)
		}

		return k, nil
	}

	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}

	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported private key type")
	}

	return s, nil
}

// LoadKeyPair reads a PEM certificate chain and its key from files. The key
// file must not be readable or writable by group or others.
func LoadKeyPair(certFile, keyFile string) (tls.Certificate, error) {
	info, err := os.Stat(keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("key file: %w", err)
	}

	if info.Mode().Perm()&0o077 != 0 {
		return tls.Certificate{}, fmt.Errorf("key file %s has mode %04o, want 0600", keyFile, info.Mode().Perm())
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load key pair: %w", err)
	}

	return cert, nil
}

// WriteFileAtomic writes data to path through a temporary file in the same
// directory and a rename, so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()

	if err := f.Chmod(perm); err != nil {
		_ = f.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
