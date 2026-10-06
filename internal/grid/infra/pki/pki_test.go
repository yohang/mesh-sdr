package pki_test

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func newCA(t *testing.T) *pki.CA {
	t.Helper()

	certPEM, keyPEM, err := pki.GenerateCA("test CA", now)
	if err != nil {
		t.Fatal(err)
	}

	ca, err := pki.ParseCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	return ca
}

func TestCAAndKeyMustMatch(t *testing.T) {
	c1, _, _ := pki.GenerateCA("a", now)
	_, k2, _ := pki.GenerateCA("b", now)

	if _, err := pki.ParseCA(c1, k2); !errors.Is(err, pki.ErrInvalidCertificate) {
		t.Fatalf("err = %v, want invalid_certificate", err)
	}
}

func TestSignNodeCSR(t *testing.T) {
	ca := newCA(t)

	key, err := pki.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	// The CSR asks for extra SANs; only the node URL host is granted.
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		URIs:     []*url.URL{pki.IdentityURI(pki.KindNode, "attic")},
		DNSNames: []string{"bank.example.com"}, IPAddresses: []net.IP{net.ParseIP("203.0.113.9")},
	}, key)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ca.SignNodeCSR(csr, "garden", "", now); !errors.Is(err, pki.ErrInvalidCertificate) {
		t.Fatalf("CSR for another node: err = %v", err)
	}

	der, err := ca.SignNodeCSR(csr, "attic", "192.0.2.10", now)
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	if kind, id, ok := pki.Identity(leaf); !ok || kind != pki.KindNode || id != "attic" {
		t.Errorf("identity = %s %s %v", kind, id, ok)
	}

	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "attic.nodes.rx.internal" ||
		len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.ParseIP("192.0.2.10")) {
		t.Errorf("SANs = %v %v", leaf.DNSNames, leaf.IPAddresses)
	}

	if got := leaf.NotAfter.Sub(now); got < 89*24*time.Hour || got > pki.NodeValidity {
		t.Errorf("validity = %v", got)
	}

	if err := pki.VerifyLeaf(leaf, ca.Pool(), x509.ExtKeyUsageServerAuth, now); err != nil {
		t.Fatal(err)
	}

	if err := pki.VerifyLeaf(leaf, ca.Pool(), x509.ExtKeyUsageServerAuth, now.Add(91*24*time.Hour)); err == nil {
		t.Error("expired leaf verified")
	}

	if pki.RenewalDue(leaf, now.Add(59*24*time.Hour)) || !pki.RenewalDue(leaf, now.Add(61*24*time.Hour)) {
		t.Error("renewal threshold is not 2/3")
	}

	renewed, err := ca.RenewNode(leaf, now.Add(61*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	r, _ := x509.ParseCertificate(renewed)
	if !leaf.PublicKey.(*ecdsa.PublicKey).Equal(r.PublicKey) || pki.SerialString(r) == pki.SerialString(leaf) {
		t.Error("renewal must keep the key and change the serial")
	}
}

func TestFingerprintRoundTrip(t *testing.T) {
	ca := newCA(t)
	s := pki.FormatFingerprint(ca.Fingerprint())

	for _, in := range []string{s, strings.ToLower(strings.ReplaceAll(s, ":", ""))} {
		fp, err := pki.ParseFingerprint(in)
		if err != nil || fp != ca.Fingerprint() {
			t.Errorf("ParseFingerprint(%q) = %x, %v", in, fp, err)
		}
	}

	if _, err := pki.ParseFingerprint("abcd"); err == nil {
		t.Error("short fingerprint accepted")
	}
}

// TestMutualTLS runs a node server and a hub client over real TLS.
func TestMutualTLS(t *testing.T) {
	ca := newCA(t)
	other := newCA(t)

	key, _ := pki.GenerateKey()
	csr, _ := pki.CreateNodeCSR(key, "attic", "")
	der, _ := ca.SignNodeCSR(csr, "attic", "node.example.org", time.Now())
	nodeCert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	revoked := pki.NewRevokedSet()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, id, _ := pki.PeerIdentity(r)
		_, _ = io.WriteString(w, kind+":"+id)
	}))
	srv.TLS = pki.NodeServerConfig(pki.NewCertHolder(nodeCert), ca.Pool(), revoked)
	srv.StartTLS()
	defer srv.Close()

	get := func(client *pki.ClientSource, roots *x509.CertPool, node string, check pki.LeafCheck) (string, error) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.HubDialConfig(client, roots, node, check)}}

		resp, err := c.Get(srv.URL)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()

		b, err := io.ReadAll(resp.Body)

		return string(b), err
	}

	hub := pki.NewClientSource(ca, pki.KindHub, "hub.example.org", time.Now)

	body, err := get(hub, ca.Pool(), "attic", nil)
	if err != nil {
		t.Fatal(err)
	}

	if body != "hub:hub.example.org" {
		t.Errorf("node saw %q", body)
	}

	if _, err := get(hub, ca.Pool(), "garden", nil); err == nil {
		t.Error("dial to the wrong node id succeeded")
	}

	pinErr := errors.New("pin mismatch")
	if _, err := get(hub, ca.Pool(), "attic", func(*x509.Certificate) error { return pinErr }); !errors.Is(err, pinErr) {
		t.Errorf("leaf check not applied: %v", err)
	}

	stranger := pki.NewClientSource(other, pki.KindHub, "evil", time.Now)
	if _, err := get(stranger, ca.Pool(), "attic", nil); err == nil {
		t.Error("client from another CA accepted")
	}

	c, _ := hub.Get()
	revoked.Add(pki.SerialString(c.Leaf))

	if _, err := get(hub, ca.Pool(), "attic", nil); err == nil {
		t.Error("revoked client accepted")
	}

	// A TLS 1.2 client is refused.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", srv.Listener.Addr().String(),
		&tls.Config{MaxVersion: tls.VersionTLS12, InsecureSkipVerify: true}) //nolint:gosec // test of the refusal
	if err == nil {
		_ = conn.Close()
		t.Error("TLS 1.2 accepted")
	}

}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tls", "node.key")
	if err := pki.WriteFileAtomic(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode())
	}
}

func TestSelfSigned(t *testing.T) {
	key, _ := pki.GenerateKey()

	cert, err := pki.SelfSigned(key, "attic", "node.example.org:8074", now)
	if err != nil {
		t.Fatal(err)
	}

	if kind, id, _ := pki.Identity(cert.Leaf); kind != pki.KindNode || id != "attic" {
		t.Errorf("identity %s %s", kind, id)
	}

	if len(cert.Leaf.DNSNames) != 1 || cert.Leaf.DNSNames[0] != "node.example.org" {
		t.Errorf("DNS = %v", cert.Leaf.DNSNames)
	}
}

func TestWriteFileExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.key")

	if err := pki.WriteFileExclusive(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := pki.WriteFileExclusive(path, []byte("two"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second write = %v, want ErrExist", err)
	}

	if b, _ := os.ReadFile(path); string(b) != "one" {
		t.Errorf("content = %q", b)
	}

	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
}

func TestWriteFilesAtomicLeavesTargetsOnFailure(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")

	if err := os.WriteFile(a, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The second file cannot be staged: nothing is renamed.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := pki.WriteFilesAtomic(
		pki.File{Path: a, Data: []byte("new"), Perm: 0o600},
		pki.File{Path: filepath.Join(blocker, "b"), Data: []byte("x"), Perm: 0o600},
	)
	if err == nil {
		t.Fatal("batch with an unwritable file succeeded")
	}

	if b, _ := os.ReadFile(a); string(b) != "old" {
		t.Errorf("a = %q, want it untouched", b)
	}

	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("temporary files left: %v", entries)
	}
}
