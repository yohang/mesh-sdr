package enroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

// LocalNodes is the node registry the in-process enrollment updates
// (grid/app.Nodes).
type LocalNodes interface {
	Get(ctx context.Context, id string) (*domain.Node, error)
	EnrollLocal(ctx context.Context, id string, cert domain.CertInfo) error
}

// LocalOptions configure Local.
type LocalOptions struct {
	// ID and Listen are node.id and node.listen of the local node.
	ID, Listen  string
	CA          *pki.CA
	Nodes       LocalNodes
	Revocations domain.RevocationRepository
	// Paths are the local node files; CA is empty when the node reads the
	// hub CA file itself.
	Paths Paths
	Now   func() time.Time
}

// Local enrolls the local node of the all role in-process (GRID-014): it
// issues the node certificate from the hub CA unless the node files hold a
// valid certificate for the enrolled node (first start, lost files,
// re-enrollment). It returns the issued certificate, if any, and
// domain.ErrNodeRevoked for a revoked node.
func Local(ctx context.Context, o LocalOptions) (domain.CertInfo, bool, error) {
	n, err := o.Nodes.Get(ctx, o.ID)
	if err != nil {
		return domain.CertInfo{}, false, err
	}

	if n.Enrollment() == domain.EnrollmentRevoked {
		return domain.CertInfo{}, false, domain.ErrNodeRevoked
	}

	key, err := localKey(o.Paths.Key)
	if err != nil {
		return domain.CertInfo{}, false, err
	}

	if validLocalCert(ctx, o, n, key) {
		return domain.CertInfo{}, false, nil
	}

	csr, err := pki.CreateNodeCSR(key, o.ID, o.Listen)
	if err != nil {
		return domain.CertInfo{}, false, err
	}

	der, err := o.CA.SignNodeCSR(csr, o.ID, n.URL().Host(), o.Now())
	if err != nil {
		return domain.CertInfo{}, false, err
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return domain.CertInfo{}, false, err
	}

	fp := pki.Fingerprint(der)

	info, err := domain.NewCertInfo(fp[:], pki.SerialString(leaf), leaf.NotAfter)
	if err != nil {
		return domain.CertInfo{}, false, err
	}

	// Files first: a crash after them leaves a certificate the database
	// does not pin, which the next start replaces.
	var caDER []byte
	if o.Paths.CA != "" {
		caDER = o.CA.Certificate().Raw
	}

	if err := WriteFiles(o.Paths, key, caDER, der); err != nil {
		return domain.CertInfo{}, false, err
	}

	if err := o.Nodes.EnrollLocal(ctx, o.ID, info); err != nil {
		return domain.CertInfo{}, false, err
	}

	return info, true, nil
}

// validLocalCert reports whether the node certificate file belongs to key,
// chains to the hub CA, names the node and is the one the hub pins, not
// revoked nor expired.
func validLocalCert(ctx context.Context, o LocalOptions, n *domain.Node, key *ecdsa.PrivateKey) bool {
	pemData, err := os.ReadFile(o.Paths.Cert)
	if err != nil {
		return false
	}

	certs, err := pki.ParseCertsPEM(pemData)
	if err != nil || len(certs) == 0 {
		return false
	}

	leaf := certs[0]

	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return false
	}

	if err := pki.VerifyLeaf(leaf, o.CA.Pool(), x509.ExtKeyUsageServerAuth, o.Now()); err != nil {
		return false
	}

	if kind, id, ok := pki.Identity(leaf); !ok || kind != pki.KindNode || id != n.ID().String() {
		return false
	}

	if n.Enrollment() != domain.EnrollmentEnrolled || !n.AcceptsFingerprint(pki.Fingerprint(leaf.Raw)) {
		return false
	}

	revoked, err := o.Revocations.List(ctx, o.Now())
	if err != nil {
		return false
	}

	for _, r := range revoked {
		if r.Serial() == pki.SerialString(leaf) {
			return false
		}
	}

	return true
}

// localKey loads the node key, or generates one when the file is absent.
func localKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return pki.GenerateKey()
	}

	if err != nil {
		return nil, fmt.Errorf("tls.key: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("tls.key: %w", err)
	}

	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("tls.key %s has mode %04o, want 0600", path, info.Mode().Perm())
	}

	signer, err := pki.ParseKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("tls.key: %w", err)
	}

	key, ok := signer.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("tls.key: not an ECDSA key")
	}

	return key, nil
}
