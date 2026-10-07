package wire

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

// EnsureCA creates the hub CA at certPath and keyPath when both are absent
// (first start of the all role, TECHNICAL_SPEC §7.4); it never replaces
// one. It reports whether it created them.
func EnsureCA(certPath, keyPath string, now time.Time) (bool, error) {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)

	switch {
	case certErr == nil && keyErr == nil:
		return false, nil
	case !errors.Is(certErr, fs.ErrNotExist) || !errors.Is(keyErr, fs.ErrNotExist):
		return false, fmt.Errorf("hub CA: %s and %s must both exist or both be absent", certPath, keyPath)
	}

	certPEM, keyPEM, err := pki.GenerateCA("MeshSDR hub CA", now)
	if err != nil {
		return false, err
	}

	if err := pki.WriteFileExclusive(keyPath, keyPEM, 0o600); err != nil {
		return false, err
	}

	if err := pki.WriteFileExclusive(certPath, certPEM, 0o644); err != nil {
		_ = os.Remove(keyPath)

		return false, err
	}

	return true, nil
}

// AllProcess is the all role: the hub and its local node in one process
// (GRID-003, GRID-014). The local node is a normal grid node, declared in
// the hub with origin config, reached over loopback mTLS by the control
// channel and through the gateway like any other node.
type AllProcess struct {
	hub     *Process
	g       *hubGrid
	nodeCfg config.Node
	logger  *slog.Logger
	root    *slog.Logger
	now     func() time.Time
}

// All builds the all role. The hub CA must exist (EnsureCA).
func All(ctx context.Context, hubCfg config.Hub, origins config.Origins, nodeCfg config.Node, logger *slog.Logger, adapter *db.DB) (*AllProcess, error) {
	id, err := griddomain.NewNodeID(nodeCfg.Node.ID)
	if err != nil {
		return nil, fmt.Errorf("node.id: %w", err)
	}

	if hubCfg.TLS.CACert == "" {
		return nil, errors.New("the all role needs the hub CA (tls.ca_cert)")
	}

	if _, ok := hubCfg.Nodes[id.String()]; ok {
		return nil, fmt.Errorf("nodes.%s: this id is the local node of the all role (node.id)", id)
	}

	url, err := localURL(nodeCfg.Node.Listen)
	if err != nil {
		return nil, err
	}

	hubID, err := HubID(hubCfg.Hub.URL)
	if err != nil {
		return nil, err
	}

	if nodeCfg.HubTrust.HubIdentity == "" {
		nodeCfg.HubTrust.HubIdentity = hubID
	}

	nodes := make(map[string]config.ConfigNode, len(hubCfg.Nodes)+1)
	for k, v := range hubCfg.Nodes {
		nodes[k] = v
	}

	nodes[id.String()] = config.ConfigNode{URL: url}
	hubCfg.Nodes = nodes

	hub, g, err := newHub(ctx, hubCfg, origins, logger, adapter, time.Now, gridapp.DefaultTimings())
	if err != nil {
		return nil, err
	}

	return &AllProcess{hub: hub, g: g, nodeCfg: nodeCfg, logger: component(logger, "wire.all"), root: logger, now: time.Now}, nil
}

// Hub returns the hub process.
func (a *AllProcess) Hub() *Process { return a.hub }

// localURL is the URL the hub dials for a node listening on listen: a
// wildcard host becomes the loopback address.
func localURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("node.listen: %w", err)
	}

	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}

	return "https://" + net.JoinHostPort(host, port), nil
}

// Run starts the hub (declaring the local node), enrolls the local node
// in-process when needed, then serves the hub and the node until ctx is
// done.
func (a *AllProcess) Run(ctx context.Context) error {
	if err := a.hub.runStartup(ctx); err != nil {
		return err
	}

	runNode := true

	if err := a.enrollLocal(ctx); err != nil {
		if !errors.Is(err, griddomain.ErrNodeRevoked) {
			return err
		}

		runNode = false

		a.logger.ErrorContext(ctx, "the local node is revoked: it is not started; issue a new enrollment token "+
			"(meshsdr hub node token "+a.nodeCfg.Node.ID+") to re-enroll it at the next start")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	run := func(f func(context.Context) error) {
		wg.Go(func() {
			if err := f(ctx); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}

			cancel()
		})
	}

	run(a.hub.Run)

	if runNode {
		node, err := Node(a.nodeCfg, a.root, a.now())
		if err != nil {
			cancel()
			wg.Wait()

			return err
		}

		run(node.Run)
	}

	wg.Wait()

	return errors.Join(errs...)
}

// enrollLocal issues the local node certificate from the hub CA unless the
// node files hold a valid certificate for the enrolled node (first start,
// lost files, re-enrollment).
func (a *AllProcess) enrollLocal(ctx context.Context) error {
	cfg := a.nodeCfg
	id := cfg.Node.ID

	n, err := a.g.nodes.Get(ctx, id)
	if err != nil {
		return err
	}

	if n.Enrollment() == griddomain.EnrollmentRevoked {
		return griddomain.ErrNodeRevoked
	}

	key, err := localKey(cfg.TLS.Key)
	if err != nil {
		return err
	}

	if a.validLocalCert(ctx, n, key) {
		return nil
	}

	csr, err := pki.CreateNodeCSR(key, id, cfg.Node.Listen)
	if err != nil {
		return err
	}

	der, err := a.g.ca.SignNodeCSR(csr, id, n.URL().Host(), a.now())
	if err != nil {
		return err
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}

	fp := pki.Fingerprint(der)

	info, err := griddomain.NewCertInfo(fp[:], pki.SerialString(leaf), leaf.NotAfter)
	if err != nil {
		return err
	}

	// Files first: a crash after them leaves a certificate the database
	// does not pin, which the next start replaces.
	paths := enroll.Paths{Key: cfg.TLS.Key, Cert: cfg.TLS.Cert, CA: cfg.HubTrust.CACert}
	if cfg.HubTrust.CACert == a.hubCACert() {
		paths.CA = ""
	}

	if err := writeLocalFiles(paths, key, der, a.g.ca); err != nil {
		return err
	}

	if err := a.g.nodes.EnrollLocal(ctx, id, info); err != nil {
		return err
	}

	a.logger.InfoContext(ctx, "local node enrolled in-process", slog.String("node_id", id), slog.String("cert_serial", info.Serial()))

	return nil
}

func (a *AllProcess) hubCACert() string { return a.g.caPath }

// validLocalCert reports whether the node certificate file belongs to key,
// chains to the hub CA, names the node and is the one the hub pins, not
// revoked nor expired.
func (a *AllProcess) validLocalCert(ctx context.Context, n *griddomain.Node, key *ecdsa.PrivateKey) bool {
	pemData, err := os.ReadFile(a.nodeCfg.TLS.Cert)
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

	if err := pki.VerifyLeaf(leaf, a.g.ca.Pool(), x509.ExtKeyUsageServerAuth, a.now()); err != nil {
		return false
	}

	if kind, id, ok := pki.Identity(leaf); !ok || kind != pki.KindNode || id != n.ID().String() {
		return false
	}

	if n.Enrollment() != griddomain.EnrollmentEnrolled || !n.AcceptsFingerprint(pki.Fingerprint(leaf.Raw)) {
		return false
	}

	revoked, err := a.g.revocations.List(ctx, a.now())
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

// writeLocalFiles writes the node key (0600), its certificate and, when
// the node does not share the hub CA file, a copy of the CA certificate.
func writeLocalFiles(p enroll.Paths, key *ecdsa.PrivateKey, der []byte, ca *pki.CA) error {
	keyPEM, err := pki.EncodeKeyPEM(key)
	if err != nil {
		return err
	}

	files := []pki.File{
		{Path: p.Key, Data: keyPEM, Perm: 0o600},
		{Path: p.Cert, Data: pki.EncodeCertsPEM(der), Perm: 0o644},
	}

	if p.CA != "" {
		files = append([]pki.File{{Path: p.CA, Data: ca.PEM(), Perm: 0o644}}, files...)
	}

	if err := pki.WriteFilesAtomic(files...); err != nil {
		return fmt.Errorf("write local node identity: %w", err)
	}

	return nil
}
