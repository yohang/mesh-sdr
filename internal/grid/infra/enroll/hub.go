package enroll

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

// HubClient is the hub side of the exchange: an app.Enroller signing CSRs
// with the hub CA.
type HubClient struct {
	ca  *pki.CA
	now func() time.Time
}

// NewHubClient returns the client.
func NewHubClient(ca *pki.CA, now func() time.Time) *HubClient { return &HubClient{ca: ca, now: now} }

var _ app.Enroller = (*HubClient)(nil)

// pinnedPeer remembers the node's self-signed certificate of phase 1 and
// refuses any other certificate afterwards.
type pinnedPeer struct {
	mu   sync.Mutex
	leaf []byte
}

func (p *pinnedPeer) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("node presented no certificate")
	}

	der := cs.PeerCertificates[0].Raw

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.leaf == nil {
		p.leaf = bytes.Clone(der)

		return nil
	}

	if !bytes.Equal(p.leaf, der) {
		return errors.New("node certificate changed during enrollment")
	}

	return nil
}

func (p *pinnedPeer) get() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.leaf
}

// Enroll implements app.Enroller.
func (c *HubClient) Enroll(ctx context.Context, t app.EnrollmentTarget) (domain.CertInfo, error) {
	pin := &pinnedPeer{}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// The node is not enrolled yet: its certificate is self-signed.
			// It is pinned for the exchange and bound to the proof below.
			InsecureSkipVerify: true, //nolint:gosec // pinned and authenticated by the HMAC proof (§4.2)
			VerifyConnection:   pin.verify,
		},
		ForceAttemptHTTP2: false,
	}
	defer tr.CloseIdleConnections()

	client := &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: noRedirect}
	endpoint := t.URL.Endpoint("https", "/enroll")

	nonce := make([]byte, domain.EnrollmentNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return domain.CertInfo{}, fmt.Errorf("nonce: %w", err)
	}

	// Open the connection first: the proof binds the node certificate.
	if err := probe(ctx, client, endpoint); err != nil {
		return domain.CertInfo{}, err
	}

	selfSigned := pin.get()
	if leaf, err := x509.ParseCertificate(selfSigned); err != nil {
		return domain.CertInfo{}, fmt.Errorf("%w: node certificate: %w", app.ErrEnrollmentRejected, err)
	} else if kind, id, ok := pki.Identity(leaf); !ok || kind != pki.KindNode || id != t.ID.String() {
		return domain.CertInfo{}, fmt.Errorf("%w: node certificate is not %s", app.ErrEnrollmentRejected, pki.IdentityURI(pki.KindNode, t.ID.String()))
	}

	var hello helloResponse

	err := post(ctx, client, endpoint, request{
		Phase:      PhaseHello,
		NodeID:     t.ID.String(),
		HubCAChain: encodeAll([][]byte{c.ca.Certificate().Raw}),
		Nonce:      b64.EncodeToString(nonce),
		Proof:      b64.EncodeToString(t.Key.HelloProof(nonce, t.ID, sha256.Sum256(selfSigned))),
	}, http.StatusOK, &hello)
	if err != nil {
		return domain.CertInfo{}, err
	}

	csr, err := b64.DecodeString(hello.CSR)
	if err != nil {
		return domain.CertInfo{}, fmt.Errorf("%w: invalid CSR encoding", app.ErrEnrollmentRejected)
	}

	mac, err := b64.DecodeString(hello.MAC)
	if err != nil || !domain.VerifyMAC(mac, t.Key.CSRMAC(nonce, csr)) {
		return domain.CertInfo{}, fmt.Errorf("%w: invalid CSR MAC", app.ErrEnrollmentRejected)
	}

	leafDER, err := c.ca.SignNodeCSR(csr, t.ID.String(), t.URL.Host(), c.now())
	if err != nil {
		return domain.CertInfo{}, fmt.Errorf("%w: %w", app.ErrEnrollmentRejected, err)
	}

	chain := [][]byte{leafDER, c.ca.Certificate().Raw}

	err = post(ctx, client, endpoint, request{
		Phase:            PhaseCertificate,
		Nonce:            b64.EncodeToString(nonce),
		CertificateChain: encodeAll(chain),
		MAC:              b64.EncodeToString(t.Key.ChainMAC(nonce, chain)),
	}, http.StatusNoContent, nil)
	if err != nil {
		return domain.CertInfo{}, err
	}

	return CertInfoOf(leafDER)
}

// noRedirect refuses redirects: the exchange talks to the node URL only.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// CertInfoOf returns the identity of a DER certificate.
func CertInfoOf(der []byte) (domain.CertInfo, error) {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return domain.CertInfo{}, fmt.Errorf("parse certificate: %w", err)
	}

	fp := pki.Fingerprint(der)

	return domain.NewCertInfo(fp[:], pki.SerialString(leaf), leaf.NotAfter)
}

// probe opens the TLS connection (any HTTP answer is fine).
func probe(ctx context.Context, c *http.Client, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("enroll request: %w", err)
	}

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("dial %s: %w", endpoint, err)
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxBody))
	_ = resp.Body.Close()

	return nil
}

func post(ctx context.Context, c *http.Client, endpoint string, body request, want int, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode enroll %s: %w", body.Phase, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("enroll request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("enroll %s: %w", body.Phase, err)
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return fmt.Errorf("enroll %s: read: %w", body.Phase, err)
	}

	if resp.StatusCode != want {
		if resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w: node answered %d to phase %s", app.ErrEnrollmentRejected, resp.StatusCode, body.Phase)
		}

		return fmt.Errorf("enroll %s: node answered %d", body.Phase, resp.StatusCode)
	}

	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%w: invalid %s answer: %w", app.ErrEnrollmentRejected, body.Phase, err)
		}
	}

	return nil
}
