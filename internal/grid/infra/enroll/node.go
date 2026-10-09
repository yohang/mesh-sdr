package enroll

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/http/problem"
)

// pendingTTL bounds the time between the two phases.
const pendingTTL = 60 * time.Second

// Result is what a successful enrollment delivers to the node.
type Result struct {
	// Chain is the node certificate chain, leaf first (DER).
	Chain [][]byte
	// CA is the hub CA certificate.
	CA *x509.Certificate
}

// NodeOptions configures a NodeEnroller.
type NodeOptions struct {
	ID            domain.NodeID
	Listen        string
	Key           *ecdsa.PrivateKey
	SelfSigned    tls.Certificate
	Token         domain.EnrollmentToken
	CAFingerprint [32]byte
	Now           func() time.Time
	Logger        *slog.Logger
}

// NodeEnroller is the node side of the exchange: it serves POST /enroll
// with the node's self-signed certificate and delivers the issued
// certificate once.
type NodeEnroller struct {
	o   NodeOptions
	key domain.EnrollmentKey

	mu      sync.Mutex
	pending *pending
	done    chan Result
	once    sync.Once
}

type pending struct {
	nonce   []byte
	ca      *x509.Certificate
	csr     []byte
	expires time.Time
}

// NewNodeEnroller returns the enroller.
func NewNodeEnroller(o NodeOptions) *NodeEnroller {
	return &NodeEnroller{o: o, key: o.Token.Key(), done: make(chan Result, 1)}
}

// Done delivers the result of the first successful enrollment.
func (e *NodeEnroller) Done() <-chan Result { return e.done }

func reject(w http.ResponseWriter) {
	problem.Write(w, problem.New(http.StatusForbidden, "enrollment_rejected", "enrollment rejected"))
}

// ServeEnroll serves POST /enroll (§4.2 step 3).
func (e *NodeEnroller) ServeEnroll(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
	dec.DisallowUnknownFields()

	var req request
	if err := dec.Decode(&req); err != nil {
		problem.Write(w, problem.New(http.StatusBadRequest, problem.CodeBadRequest, "invalid enrollment request"))

		return
	}

	nonce, err := b64.DecodeString(req.Nonce)
	if err != nil || len(nonce) != domain.EnrollmentNonceSize {
		problem.Write(w, problem.New(http.StatusBadRequest, problem.CodeBadRequest, "invalid nonce"))

		return
	}

	switch req.Phase {
	case PhaseHello:
		e.hello(w, r, req, nonce)
	case PhaseCertificate:
		e.certificate(w, r, req, nonce)
	default:
		problem.Write(w, problem.New(http.StatusBadRequest, problem.CodeBadRequest, "unknown phase"))
	}
}

func (e *NodeEnroller) warn(r *http.Request, reason string, err error) {
	e.o.Logger.WarnContext(r.Context(), "enrollment request rejected",
		slog.String("reason", reason), slog.String("remote_addr", r.RemoteAddr), slog.Any("error", err))
}

func (e *NodeEnroller) hello(w http.ResponseWriter, r *http.Request, req request, nonce []byte) {
	if req.NodeID != e.o.ID.String() {
		e.warn(r, "node_id mismatch", fmt.Errorf("got %q", req.NodeID))
		reject(w)

		return
	}

	chain, err := decodeAll(req.HubCAChain)
	if err != nil {
		e.warn(r, "invalid hub_ca_chain", err)
		reject(w)

		return
	}

	root := chain[len(chain)-1]
	if sha256.Sum256(root) != e.o.CAFingerprint {
		e.warn(r, "hub CA fingerprint mismatch", nil)
		reject(w)

		return
	}

	ca, err := x509.ParseCertificate(root)
	if err != nil || !ca.IsCA {
		e.warn(r, "hub CA is not a CA certificate", err)
		reject(w)

		return
	}

	proof, err := b64.DecodeString(req.Proof)
	if err != nil || !domain.VerifyMAC(proof, e.key.HelloProof(nonce, e.o.ID, sha256.Sum256(e.o.SelfSigned.Certificate[0]))) {
		e.warn(r, "invalid proof", err)
		reject(w)

		return
	}

	csr, err := pki.CreateNodeCSR(e.o.Key, e.o.ID.String(), e.o.Listen)
	if err != nil {
		e.o.Logger.ErrorContext(r.Context(), "create CSR", slog.Any("error", err))
		problem.Write(w, problem.New(http.StatusInternalServerError, problem.CodeInternal, ""))

		return
	}

	e.mu.Lock()
	e.pending = &pending{nonce: nonce, ca: ca, csr: csr, expires: e.o.Now().Add(pendingTTL)}
	e.mu.Unlock()

	e.o.Logger.InfoContext(r.Context(), "enrollment: hub authenticated, CSR sent", slog.String("remote_addr", r.RemoteAddr))

	writeJSON(w, http.StatusOK, helloResponse{CSR: b64.EncodeToString(csr), MAC: b64.EncodeToString(e.key.CSRMAC(nonce, csr))})
}

func (e *NodeEnroller) certificate(w http.ResponseWriter, r *http.Request, req request, nonce []byte) {
	e.mu.Lock()
	p := e.pending
	e.mu.Unlock()

	if p == nil || !bytes.Equal(p.nonce, nonce) || !e.o.Now().Before(p.expires) {
		e.warn(r, "no pending enrollment for this nonce", nil)
		reject(w)

		return
	}

	chain, err := decodeAll(req.CertificateChain)
	if err != nil {
		e.warn(r, "invalid certificate_chain", err)
		reject(w)

		return
	}

	mac, err := b64.DecodeString(req.MAC)
	if err != nil || !domain.VerifyMAC(mac, e.key.ChainMAC(nonce, chain)) {
		e.warn(r, "invalid certificate MAC", err)
		reject(w)

		return
	}

	if err := e.checkLeaf(chain[0], p.ca); err != nil {
		e.warn(r, "invalid certificate", err)
		reject(w)

		return
	}

	w.WriteHeader(http.StatusNoContent)

	e.once.Do(func() {
		e.mu.Lock()
		e.pending = nil
		e.mu.Unlock()

		e.done <- Result{Chain: chain, CA: p.ca}
	})
}

func (e *NodeEnroller) checkLeaf(der []byte, ca *x509.Certificate) error {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)

	if err := pki.VerifyLeaf(leaf, pool, x509.ExtKeyUsageServerAuth, e.o.Now()); err != nil {
		return err
	}

	if kind, id, ok := pki.Identity(leaf); !ok || kind != pki.KindNode || id != e.o.ID.String() {
		return errors.New("certificate identity is not this node")
	}

	if !e.o.Key.PublicKey.Equal(leaf.PublicKey) {
		return errors.New("certificate is not for this node key")
	}

	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
