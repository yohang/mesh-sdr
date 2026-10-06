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

// WriteFiles writes the hub CA, the node key (0600) and the certificate
// chain. All three are staged before any is renamed into place; the
// certificate goes last, because its presence marks the node as enrolled:
// a crash in between leaves a node that is not enrolled yet, and
// `meshsdr node enroll` rewrites the key.
func WriteFiles(p Paths, key *ecdsa.PrivateKey, res Result) error {
	keyPEM, err := pki.EncodeKeyPEM(key)
	if err != nil {
		return err
	}

	err = pki.WriteFilesAtomic(
		pki.File{Path: p.CA, Data: pki.EncodeCertsPEM(res.CA.Raw), Perm: 0o644},
		pki.File{Path: p.Key, Data: keyPEM, Perm: 0o600},
		pki.File{Path: p.Cert, Data: pki.EncodeCertsPEM(res.Chain...), Perm: 0o644},
	)
	if err != nil {
		return fmt.Errorf("write node identity: %w", err)
	}

	return nil
}
