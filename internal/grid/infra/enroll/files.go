package enroll

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

// Paths are the files written at enrollment (tls.key, tls.cert,
// hub_trust.ca_cert).
type Paths struct {
	Key, Cert, CA string
}

// WriteFiles writes the node key (0600), certificate chain and hub CA
// atomically. The key goes first, so a certificate never exists without it.
func WriteFiles(p Paths, key *ecdsa.PrivateKey, res Result) error {
	keyPEM, err := pki.EncodeKeyPEM(key)
	if err != nil {
		return err
	}

	if err := pki.WriteFileAtomic(p.Key, keyPEM, 0o600); err != nil {
		return fmt.Errorf("tls.key: %w", err)
	}

	if err := pki.WriteFileAtomic(p.CA, pki.EncodeCertsPEM(res.CA.Raw), 0o644); err != nil {
		return fmt.Errorf("hub_trust.ca_cert: %w", err)
	}

	if err := pki.WriteFileAtomic(p.Cert, pki.EncodeCertsPEM(res.Chain...), 0o644); err != nil {
		return fmt.Errorf("tls.cert: %w", err)
	}

	return nil
}
