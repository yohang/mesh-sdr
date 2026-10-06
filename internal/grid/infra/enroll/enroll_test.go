package enroll_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

var discard = slog.New(slog.DiscardHandler)

type node struct {
	enroller *enroll.NodeEnroller
	url      domain.NodeURL
	opts     enroll.NodeOptions
}

func newCA(t *testing.T) *pki.CA {
	t.Helper()

	certPEM, keyPEM, err := pki.GenerateCA("hub", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	ca, err := pki.ParseCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	return ca
}

func startNode(t *testing.T, id string, tok domain.EnrollmentToken, caFP [32]byte) *node {
	t.Helper()

	key, _ := pki.GenerateKey()

	self, err := pki.SelfSigned(key, id, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	o := enroll.NodeOptions{
		ID: domain.MustNodeID(id), Key: key, SelfSigned: self, Token: tok, CAFingerprint: caFP,
		Now: time.Now, Logger: discard,
	}
	e := enroll.NewNodeEnroller(o)

	srv := httptest.NewUnstartedServer(e.Handler())
	srv.TLS = pkiTLS(self)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return &node{enroller: e, url: domain.MustNodeURL(srv.URL), opts: o}
}

func pkiTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
}

func TestEnrollment(t *testing.T) {
	ca := newCA(t)
	tok, _ := domain.NewEnrollmentToken()
	n := startNode(t, "attic", tok, ca.Fingerprint())

	hub := enroll.NewHubClient(ca, time.Now)

	info, err := hub.Enroll(context.Background(), app.EnrollmentTarget{ID: domain.MustNodeID("attic"), URL: n.url, Key: tok.Key()})
	if err != nil {
		t.Fatal(err)
	}

	var res enroll.Result
	select {
	case res = <-n.enroller.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("node did not complete")
	}

	if sha256.Sum256(res.Chain[0]) != info.Fingerprint() || !res.CA.Equal(ca.Certificate()) {
		t.Error("node and hub disagree on the certificate")
	}

	leaf, _ := x509.ParseCertificate(res.Chain[0])
	if !n.opts.Key.PublicKey.Equal(leaf.PublicKey) {
		t.Error("certificate is not for the node key")
	}

	dir := t.TempDir()
	p := enroll.Paths{Key: filepath.Join(dir, "tls", "node.key"), Cert: filepath.Join(dir, "tls", "node.pem"), CA: filepath.Join(dir, "tls", "ca.pem")}

	if err := enroll.WriteFiles(p, n.opts.Key, res); err != nil {
		t.Fatal(err)
	}

	if _, err := pki.LoadKeyPair(p.Cert, p.Key); err != nil {
		t.Fatalf("written key pair: %v", err)
	}

	if info, _ := os.Stat(p.Key); info.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", info.Mode())
	}
}

func TestEnrollmentRejections(t *testing.T) {
	ca := newCA(t)
	tok, _ := domain.NewEnrollmentToken()
	wrong, _ := domain.NewEnrollmentToken()
	hub := enroll.NewHubClient(ca, time.Now)

	tests := []struct {
		name   string
		nodeID string
		fp     [32]byte
		target string
		key    domain.EnrollmentKey
	}{
		{"wrong token", "attic", ca.Fingerprint(), "attic", wrong.Key()},
		{"wrong CA fingerprint", "attic", newCA(t).Fingerprint(), "attic", tok.Key()},
		{"other node answers", "garden", ca.Fingerprint(), "attic", tok.Key()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := startNode(t, tt.nodeID, tok, tt.fp)

			_, err := hub.Enroll(context.Background(), app.EnrollmentTarget{ID: domain.MustNodeID(tt.target), URL: n.url, Key: tt.key})
			if !errors.Is(err, app.ErrEnrollmentRejected) {
				t.Fatalf("err = %v, want enrollment_rejected", err)
			}

			select {
			case <-n.enroller.Done():
				t.Fatal("node completed a rejected enrollment")
			default:
			}
		})
	}
}

func TestEnrollmentDoesNotFollowRedirects(t *testing.T) {
	ca := newCA(t)
	tok, _ := domain.NewEnrollmentToken()
	n := startNode(t, "attic", tok, ca.Fingerprint())

	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, n.url.String()+"/enroll", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	hub := enroll.NewHubClient(ca, time.Now)
	if _, err := hub.Enroll(context.Background(), app.EnrollmentTarget{ID: domain.MustNodeID("attic"), URL: domain.MustNodeURL(redirector.URL), Key: tok.Key()}); err == nil {
		t.Fatal("enrollment through a redirect succeeded")
	}

	select {
	case <-n.enroller.Done():
		t.Fatal("the redirect target was enrolled")
	default:
	}
}
