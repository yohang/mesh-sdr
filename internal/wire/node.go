package wire

import (
	"crypto/ecdsa"
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
)

// Enrollment is the one-off process of `meshsdr node enroll`.
type Enrollment struct {
	*Process

	Enroller *enroll.NodeEnroller
	Key      *ecdsa.PrivateKey
	Paths    enroll.Paths
}

// NodeEnrollment builds the enrollment process of a node: POST /enroll on
// node.listen with a self-signed certificate of a fresh key (§4.2 step 3).
func NodeEnrollment(cfg config.Node, logger *slog.Logger, token griddomain.EnrollmentToken, caFingerprint [32]byte, now func() time.Time) (*Enrollment, error) {
	id, err := griddomain.NewNodeID(cfg.Node.ID)
	if err != nil {
		return nil, fmt.Errorf("node.id: %w", err)
	}

	key, err := pki.GenerateKey()
	if err != nil {
		return nil, err
	}

	self, err := pki.SelfSigned(key, id.String(), cfg.Node.Listen, now())
	if err != nil {
		return nil, err
	}

	e := enroll.NewNodeEnroller(enroll.NodeOptions{
		ID: id, Listen: cfg.Node.Listen, Key: key, SelfSigned: self, Token: token,
		CAFingerprint: caFingerprint, Now: now, Logger: component(logger, "grid.infra.enroll"),
	})

	srv := httpserver.NewServer(cfg.Node.Listen, e.Handler())
	srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{self}}

	return &Enrollment{
		Process:  &Process{addr: cfg.Node.Listen, server: srv, logger: component(logger, "grid.http.server")},
		Enroller: e,
		Key:      key,
		Paths:    enroll.Paths{Key: cfg.TLS.Key, Cert: cfg.TLS.Cert, CA: cfg.HubTrust.CACert},
	}, nil
}
