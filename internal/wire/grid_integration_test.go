package wire

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

var quiet = slog.New(slog.DiscardHandler)

type fakeProber struct{ devices []ctl.Device }

func (p fakeProber) Capabilities(context.Context) ctl.Capabilities {
	return ctl.Capabilities{
		ProductVersion: "dev", Protocols: []string{"rx-ctl.v1", "rx.v1"},
		Platform:   ctl.Platform{OS: "linux", Arch: "arm64", CPUCores: 4, RAMBytes: 4 << 30},
		SDRDrivers: []ctl.SDRDriver{}, Devices: append([]ctl.Device{}, p.devices...), DevicesDetected: []any{},
		Decoders: []ctl.Decoder{}, AudioCodecs: []string{}, FFTCodecs: []string{},
	}
}

func (fakeProber) Heartbeat(context.Context) ctl.Heartbeat {
	return ctl.Heartbeat{UptimeS: 5, CPU: 0.25, Load: [3]float64{0.5, 0.4, 0.3}, Mem: ctl.Mem{TotalBytes: 4 << 30, AvailableBytes: 2 << 30}}
}

func (fakeProber) NTPSynced() bool { return true }

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// run serves p until the test ends or stop is called.
func run(t *testing.T, p *Process) (stop func()) {
	t.Helper()

	ln, err := p.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	return runOn(t, p, ln)
}

// runFront runs p behind its gateway until the test ends.
func runFront(t *testing.T, p *Process) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- p.Run(ctx) }()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})

	eventually(t, "gateway listening", 10*time.Second, func() bool {
		c, err := net.Dial("tcp4", p.Addr())
		if err == nil {
			_ = c.Close()
		}

		return err == nil
	})
}

// runOn serves p on ln (a hub without its gateway).
func runOn(t *testing.T, p *Process, ln net.Listener) (stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() { done <- p.Serve(ctx, ln) }()

	var once sync.Once

	stop = func() {
		once.Do(func() {
			cancel()

			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
		})
	}

	t.Cleanup(stop)

	return stop
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = ln.Close() }()

	return ln.Addr().String()
}

// gridEnv is a hub with its CA and database, and the config of one node.
type gridEnv struct {
	hubCfg   config.Hub
	nodeCfg  config.Node
	adapter  *db.DB
	g        *hubGrid
	nodeAddr string
	ca       *pki.CA
	// gatewayAddr is the plain listener of the gateway, when the hub runs
	// behind it.
	gatewayAddr string
}

func newGridEnv(t *testing.T, timings gridapp.Timings, tweaks ...func(*control.HubOptions)) *gridEnv {
	t.Helper()

	return newGridEnvWith(t, false, timings, tweaks...)
}

// newGridEnvWith builds the env; withGateway runs the hub behind its
// embedded gateway (plain HTTP on a free port, hub.url on it).
func newGridEnvWith(t *testing.T, withGateway bool, timings gridapp.Timings, tweaks ...func(*control.HubOptions)) *gridEnv {
	t.Helper()

	if !GatewayAvailable() {
		t.Skip("the hub needs the gateway (nogateway build)")
	}

	hubDir, nodeDir := t.TempDir(), t.TempDir()

	hubURL, hubID, gatewayAddr, gatewayCfg := "https://hub.example.org", "hub.example.org", "", ""
	if withGateway {
		gatewayAddr = freePort(t)
		hubURL, hubID = "http://"+gatewayAddr, "127.0.0.1"
		gatewayCfg = "[gateway]\ntls_mode = \"off\"\nhttp_listen = \"" + gatewayAddr + "\"\nstorage_dir = \"" +
			filepath.Join(hubDir, "caddy") + "\"\n"
	}

	certPEM, keyPEM, err := pki.GenerateCA("test hub CA", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	ca, _ := pki.ParseCA(certPEM, keyPEM)

	writeFile(t, filepath.Join(hubDir, "tls", "ca.pem"), string(certPEM), 0o644)
	writeFile(t, filepath.Join(hubDir, "tls", "ca.key"), string(keyPEM), 0o600)
	writeFile(t, filepath.Join(hubDir, "hub.toml"), `schema_version = 1
`+gatewayCfg+`[hub]
url = "`+hubURL+`"
allow_insecure_url = true
[db]
dsn = "sqlite://`+filepath.Join(hubDir, "hub.db")+`"
[tls]
ca_cert = "tls/ca.pem"
ca_key = { file = "tls/ca.key" }
`, 0o600)

	hubCfg, _, err := config.LoadHub(config.Options{Dir: hubDir, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	nodeAddr := freePort(t)
	writeFile(t, filepath.Join(nodeDir, "node.toml"), `schema_version = 1
[node]
id = "attic"
listen = "`+nodeAddr+`"
[tls]
cert = "tls/node.pem"
key = "tls/node.key"
[hub_trust]
ca_cert = "tls/ca.pem"
hub_identity = "`+hubID+`"
ca_fingerprint = "`+pki.FormatFingerprint(ca.Fingerprint())+`"
[devices.hf]
name = "HF"
type = "rtl_sdr"
freq_range = { min = "100kHz", max = 30_000_000 }
sample_rates = [2_048_000]
`, 0o600)

	nodeCfg, _, err := config.LoadNode(config.Options{Dir: nodeDir, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	hubCfg.Auth.TokenKeyDir = filepath.Join(t.TempDir(), "keys")

	adapter, err := OpenDB(ctx, hubCfg.DB, quiet)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = adapter.Close() })

	if _, err := adapter.Migrator().Up(ctx); err != nil {
		t.Fatal(err)
	}

	p, g, err := newHub(context.Background(), hubCfg, config.Origins{}, quiet, adapter, time.Now, timings, tweaks...)
	if err != nil {
		t.Fatal(err)
	}

	if withGateway {
		runFront(t, p)
	} else {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		runOn(t, p, ln)
	}

	return &gridEnv{hubCfg: hubCfg, nodeCfg: nodeCfg, adapter: adapter, g: g, nodeAddr: nodeAddr, ca: ca, gatewayAddr: gatewayAddr}
}

// enrollNode adds the node on the hub and runs `node enroll` until the hub
// has enrolled it, then starts the enrolled node.
func (e *gridEnv) enrollNode(t *testing.T, prober fakeProber, opts ...NodeOption) (stop func()) {
	t.Helper()

	ctx := context.Background()

	issued, err := e.g.nodes.Add(ctx, gridapp.ActorCLI, gridapp.NewNodeInput{ID: "attic", URL: "https://" + e.nodeAddr})
	if err != nil {
		t.Fatal(err)
	}

	return e.enrollWith(t, issued.Token, issued.CAFingerprint, prober, opts...)
}

// enrollWith runs `node enroll` with a token the hub issued until the hub
// has enrolled the node, then starts the enrolled node.
func (e *gridEnv) enrollWith(t *testing.T, token domain.EnrollmentToken, caFingerprint string, prober fakeProber, opts ...NodeOption) (stop func()) {
	t.Helper()

	ctx := context.Background()
	fp, _ := pki.ParseFingerprint(caFingerprint)

	en, err := NodeEnrollment(e.nodeCfg, quiet, token, fp, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	ectx, cancel := context.WithCancel(ctx)
	runErr := make(chan error, 1)

	go func() { runErr <- en.Run(ectx) }()

	select {
	case res := <-en.Enroller.Done():
		if err := enroll.WriteFiles(en.Paths, en.Key, res); err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("node was not enrolled")
	}

	cancel()

	if err := <-runErr; err != nil {
		t.Fatal(err)
	}

	return e.startNode(t, prober, opts...)
}

func (e *gridEnv) startNode(t *testing.T, prober fakeProber, opts ...NodeOption) (stop func()) {
	t.Helper()

	if prober.devices == nil {
		prober.devices = DevicesOf(e.nodeCfg)
	}

	np, err := Node(e.nodeCfg, quiet, time.Now(), append([]NodeOption{WithProber(prober)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}

	return run(t, np)
}

func (e *gridEnv) node(t *testing.T) *domain.Node {
	t.Helper()

	n, err := gridsqlite.NewNodeRepository(e.adapter).Get(context.Background(), domain.MustNodeID("attic"))
	if err != nil {
		t.Fatal(err)
	}

	return n
}

func fastTimings() gridapp.Timings {
	t := gridapp.DefaultTimings()
	t.HeartbeatInterval = 100 * time.Millisecond
	t.OfflineAfter = 2 * time.Second

	return t
}

// TestGridEndToEnd enrolls a node over real TLS, then runs the control
// channel over real mTLS between a hub and a node process.
func TestGridEndToEnd(t *testing.T) {
	e := newGridEnv(t, fastTimings())
	e.enrollNode(t, fakeProber{})

	ctx := context.Background()
	id := domain.MustNodeID("attic")

	n := e.node(t)
	if n.Enrollment() != domain.EnrollmentEnrolled || n.Certificate().IsZero() {
		t.Fatalf("node after enrollment = %+v", n.Snapshot())
	}

	// The control channel comes up and node events are persisted and acked.
	eventually(t, "control channel", 15*time.Second, func() bool { return e.g.manager.Connected(id) })

	var boot = e.node(t).Runtime().BootID

	eventually(t, "welcome recorded", 5*time.Second, func() bool {
		boot = e.node(t).Runtime().BootID

		return !boot.IsZero()
	})

	cursors := gridsqlite.NewCursorRepository(e.adapter)
	eventually(t, "events ingested", 5*time.Second, func() bool {
		seq, err := cursors.Last(ctx, id, boot)

		return err == nil && seq >= 2 // capabilities + heartbeats
	})

	// The capability report is stored, with the platform in the node row.
	eventually(t, "capabilities", 5*time.Second, func() bool {
		rep, err := e.g.caps.Get(ctx, "attic")

		return err == nil && rep.ProductVersion() == "dev" && len(rep.Protocols()) == 2
	})

	if rt := e.node(t).Runtime(); rt.CPUCores != 4 {
		t.Errorf("cpu cores = %d", rt.CPUCores)
	}

	// The node config devices are mirrored into the registry.
	eventually(t, "device registry", 5*time.Second, func() bool {
		d, err := e.g.devices.Get(ctx, "hf")
		if err != nil {
			return false
		}

		lo, hi := d.FreqRange()

		return d.Node() == id && d.Type() == "rtl_sdr" && lo == 100_000 && hi == 30_000_000
	})

	if err := e.g.caps.Probe(ctx, "attic"); err != nil {
		t.Errorf("probe: %v", err)
	}

	// The node API accepts only certificates of the hub CA, and /control
	// only the hub identity.
	gateway := pki.NewClientSource(e.ca, pki.KindGateway, "hub.example.org", time.Now)
	if status := nodeGet(t, e, gateway, "/control"); status != http.StatusForbidden {
		t.Errorf("gateway on /control = %d, want 403", status)
	}

	if status := nodeGet(t, e, gateway, "/healthz/live"); status != http.StatusOK {
		t.Errorf("healthz = %d", status)
	}

	otherPEM, otherKey, _ := pki.GenerateCA("other", time.Now())
	other, _ := pki.ParseCA(otherPEM, otherKey)

	if status := nodeGet(t, e, pki.NewClientSource(other, pki.KindHub, "hub.example.org", time.Now), "/healthz/live"); status != 0 {
		t.Errorf("foreign CA accepted: %d", status)
	}

	// Deleting the node closes its channel and revokes its certificate.
	if err := e.g.nodes.Delete(ctx, gridapp.ActorCLI, "attic"); err != nil {
		t.Fatal(err)
	}

	eventually(t, "channel closed", 5*time.Second, func() bool { return !e.g.manager.Connected(id) })

	revoked, _ := gridsqlite.NewRevocationRepository(e.adapter).List(ctx, time.Now())
	if len(revoked) != 1 || revoked[0].Serial() != n.Certificate().Serial() {
		t.Errorf("revocation list = %+v", revoked)
	}
}

// nodeGet calls the enrolled node API with a client certificate; it
// returns 0 when the TLS handshake fails.
func nodeGet(t *testing.T, e *gridEnv, client *pki.ClientSource, path string) int {
	t.Helper()

	cfg := pki.HubDialConfig(client, e.ca.Pool(), "attic", nil)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+e.nodeAddr+path, nil)

	resp, err := c.Do(req)
	if err != nil {
		var te *tls.CertificateVerificationError
		_ = errors.As(err, &te)

		return 0
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

// TestNodeStatusLifecycle follows a node through online, offline when its
// process stops, and online again with a new boot when it restarts.
func TestNodeStatusLifecycle(t *testing.T) {
	e := newGridEnv(t, fastTimings())
	stop := e.enrollNode(t, fakeProber{})

	id := domain.MustNodeID("attic")

	eventually(t, "online", 15*time.Second, func() bool { return e.node(t).Runtime().Status == domain.StatusOnline })

	firstBoot := e.node(t).Runtime().BootID

	eventually(t, "load history", 5*time.Second, func() bool { return len(e.g.history.Samples(id)) >= 2 })

	stop()

	eventually(t, "offline", 10*time.Second, func() bool { return e.node(t).Runtime().Status == domain.StatusOffline })

	e.startNode(t, fakeProber{})

	eventually(t, "online again", 15*time.Second, func() bool {
		rt := e.node(t).Runtime()

		return rt.Status == domain.StatusOnline && rt.BootID != firstBoot
	})
}

// TestCertificateRenewal renews the node certificate over the control
// channel; the node keeps working with the new certificate after a restart.
func TestCertificateRenewal(t *testing.T) {
	var checks atomic.Int32

	e := newGridEnv(t, fastTimings(), func(o *control.HubOptions) {
		// Not due when the channel opens, due at the first periodic check
		// of the open channel (the daily check, here every 50 ms).
		o.RenewCheckEvery = 50 * time.Millisecond
		o.RenewalDue = func(*x509.Certificate, time.Time) bool { return checks.Add(1) == 2 }
	})
	stop := e.enrollNode(t, fakeProber{})

	id := domain.MustNodeID("attic")
	first := e.node(t).Certificate()

	eventually(t, "renewed certificate promoted", 15*time.Second, func() bool {
		n := e.node(t)

		return n.Certificate().Serial() != first.Serial() && n.PendingCertificate().IsZero()
	})

	if cert, err := pki.LoadKeyPair(e.nodeCfg.TLS.Cert, e.nodeCfg.TLS.Key); err != nil ||
		pki.Fingerprint(cert.Certificate[0]) != e.node(t).Certificate().Fingerprint() {
		t.Fatalf("node file does not hold the renewed certificate: %v", err)
	}

	stop()
	e.startNode(t, fakeProber{})

	eventually(t, "reconnected with the renewed certificate", 15*time.Second, func() bool {
		return e.g.manager.Connected(id) && e.node(t).Runtime().Status == domain.StatusOnline
	})
}

// TestRenewalSurvivesLostAck: the node installed a renewed certificate but
// the hub never recorded the acknowledgement. The pending certificate is
// accepted, so the node reconnects and the renewal is confirmed.
func TestRenewalSurvivesLostAck(t *testing.T) {
	e := newGridEnv(t, fastTimings())
	stop := e.enrollNode(t, fakeProber{})
	ctx := context.Background()
	id := domain.MustNodeID("attic")

	eventually(t, "connected", 15*time.Second, func() bool { return e.g.manager.Connected(id) })

	old, err := pki.LoadKeyPair(e.nodeCfg.TLS.Cert, e.nodeCfg.TLS.Key)
	if err != nil {
		t.Fatal(err)
	}

	leaf, _ := x509.ParseCertificate(old.Certificate[0])

	der, err := e.ca.RenewNode(leaf, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	info, err := enroll.CertInfoOf(der)
	if err != nil {
		t.Fatal(err)
	}

	// The hub sent the certificate (pending) and the node installed it;
	// the ack is lost when the node stops.
	if err := e.g.control.ProposeRenewal(ctx, id, info); err != nil {
		t.Fatal(err)
	}

	stop()

	if err := pki.WriteFileAtomic(e.nodeCfg.TLS.Cert, pki.EncodeCertsPEM(der, e.ca.Certificate().Raw), 0o644); err != nil {
		t.Fatal(err)
	}

	e.startNode(t, fakeProber{})

	eventually(t, "reconnected and renewal confirmed", 15*time.Second, func() bool {
		n := e.node(t)

		return e.g.manager.Connected(id) && n.Certificate().Serial() == info.Serial() && n.PendingCertificate().IsZero()
	})
}
