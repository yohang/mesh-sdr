package wire

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/grid/agent"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/media"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/grid/infra/probe"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/version"
)

// NodeOption customises the node graph (tests).
type NodeOption func(*nodeOptions)

type nodeOptions struct {
	prober  agent.Prober
	devices func() []ctl.Device
}

// WithProber replaces the host prober.
func WithProber(p agent.Prober) NodeOption { return func(o *nodeOptions) { o.prober = p } }

// NodeEnrolled reports whether the node has its certificate (tls.cert).
func NodeEnrolled(cfg config.Node) bool {
	if cfg.TLS.Cert == "" {
		return false
	}

	_, err := os.Stat(cfg.TLS.Cert)

	return err == nil
}

// enrolledNode builds an enrolled node: the mTLS node API with /control.
func enrolledNode(cfg config.Node, id griddomain.NodeID, logger *slog.Logger, opts ...NodeOption) (*Process, error) {
	var o nodeOptions
	for _, opt := range opts {
		opt(&o)
	}

	cert, err := pki.LoadKeyPair(cfg.TLS.Cert, cfg.TLS.Key)
	if err != nil {
		return nil, fmt.Errorf("tls.cert / tls.key: %w", err)
	}

	caPEM, err := os.ReadFile(cfg.HubTrust.CACert)
	if err != nil {
		return nil, fmt.Errorf("hub_trust.ca_cert: %w", err)
	}

	ca, err := pki.ParseCACert(caPEM)
	if err != nil {
		return nil, fmt.Errorf("hub_trust.ca_cert: %w", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca)

	key, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("tls.key: unsupported key type")
	}

	if o.devices == nil {
		devices := DevicesOf(cfg)
		o.devices = func() []ctl.Device { return devices }
	}

	if o.prober == nil {
		o.prober = probe.New(version.String(), o.devices, time.Now())
	}

	ag, err := agent.New(agent.Options{
		NodeID: id.String(), Version: version.String(),
		Buffer: agent.NewBuffer(cfg.Node.EventBuffer.MaxEvents, int(cfg.Node.EventBuffer.MaxBytes.Bytes())),
		Prober: o.prober, Now: time.Now, Logger: component(logger, "grid.agent"),
	})
	if err != nil {
		return nil, err
	}

	holder := pki.NewCertHolder(cert)
	revoked := pki.NewRevokedSet()

	mediaServer := media.NewServer(media.Options{
		NodeID: id.String(), Version: version.String(), GatewayIdentity: cfg.HubTrust.HubIdentity,
		OwnSerial: holder.Serial, Agent: ag, Now: time.Now, Logger: component(logger, "grid.infra.media"),
	})

	ctlServer := control.NewNodeServer(control.NodeOptions{
		Agent: ag, Media: mediaServer, HubIdentity: cfg.HubTrust.HubIdentity, Revoked: revoked,
		Renewer: &control.FileRenewer{NodeID: id.String(), CertFile: cfg.TLS.Cert, Roots: roots, Key: key, Holder: holder, Now: time.Now},
		Now:     time.Now, Logger: component(logger, "grid.infra.control"),
	})

	srv := httpserver.NewServer(cfg.Node.Listen, gridhttp.NewNodeRouter(ctlServer, mediaServer, component(logger, "grid.http.node")))
	srv.TLSConfig = pki.NodeServerConfig(holder, roots, revoked)
	// Refused handshakes (foreign CA, revoked peer) are expected noise.
	srv.ErrorLog = slog.NewLogLogger(component(logger, "grid.http.server").Handler(), slog.LevelDebug)

	return &Process{
		addr: cfg.Node.Listen, server: srv, logger: component(logger, "grid.http.server"),
		workers: []func(context.Context){ag.Run, ctlServer.Run, mediaServer.Run},
	}, nil
}

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
	srv.ErrorLog = slog.NewLogLogger(component(logger, "grid.http.server").Handler(), slog.LevelDebug)
	srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{self}}

	return &Enrollment{
		Process:  &Process{addr: cfg.Node.Listen, server: srv, logger: component(logger, "grid.http.server")},
		Enroller: e,
		Key:      key,
		Paths:    enroll.Paths{Key: cfg.TLS.Key, Cert: cfg.TLS.Cert, CA: cfg.HubTrust.CACert},
	}, nil
}

// DevicesOf lists the [devices.<id>] of the node config, ordered by id
// (TOML tables carry no order once decoded).
func DevicesOf(cfg config.Node) []ctl.Device {
	out := make([]ctl.Device, 0, len(cfg.Devices))

	for _, id := range slices.Sorted(maps.Keys(cfg.Devices)) {
		d := cfg.Devices[id]
		out = append(out, ctl.Device{
			ID: id, Name: d.Name, Type: d.Type, Enabled: d.Enabled == nil || *d.Enabled,
			FreqMin: d.FreqRange.Min.Hz(), FreqMax: d.FreqRange.Max.Hz(), SampleRates: slices.Clone(d.SampleRates),
			ListenPolicy: d.ListenPolicy, OperatorCanRetune: d.OperatorCanRetune, AlwaysOn: d.AlwaysOn, SchedulerEnabled: d.SchedulerEnabled,
		})
	}

	return out
}
