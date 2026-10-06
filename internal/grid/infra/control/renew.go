package control

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

// FileRenewer verifies a renewed node certificate, writes it to tls.cert
// atomically (the documented exception of ADR 0008 Q13) and swaps it in.
type FileRenewer struct {
	NodeID   string
	CertFile string
	Roots    *x509.CertPool
	Key      crypto.Signer
	Holder   *pki.CertHolder
	Now      func() time.Time
}

// Renew implements Renewer.
func (r *FileRenewer) Renew(chain [][]byte) error {
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return fmt.Errorf("parse renewed certificate: %w", err)
	}

	if err := pki.VerifyLeaf(leaf, r.Roots, x509.ExtKeyUsageServerAuth, r.Now()); err != nil {
		return err
	}

	if kind, id, ok := pki.Identity(leaf); !ok || kind != pki.KindNode || id != r.NodeID {
		return errors.New("renewed certificate is not for this node")
	}

	pub, ok := r.Key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(leaf.PublicKey) {
		return errors.New("renewed certificate is not for this node key")
	}

	if err := pki.WriteFileAtomic(r.CertFile, pki.EncodeCertsPEM(chain...), 0o644); err != nil {
		return err
	}

	r.Holder.Set(tls.Certificate{Certificate: chain, PrivateKey: r.Key, Leaf: leaf})

	return nil
}
