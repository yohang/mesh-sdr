package wire

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

// allEnv is the config dir of an all-role installation.
type allEnv struct {
	dir     string
	gateway string
}

func newAllEnv(t *testing.T) *allEnv {
	t.Helper()

	if !GatewayAvailable() {
		t.Skip("the all role needs the gateway (nogateway build)")
	}

	dir := t.TempDir()
	gw := freePort(t)

	writeFile(t, filepath.Join(dir, "hub.toml"), `schema_version = 1
[hub]
url = "http://`+gw+`"
allow_insecure_url = true
[gateway]
tls_mode = "off"
http_listen = "`+gw+`"
storage_dir = "`+filepath.Join(dir, "caddy")+`"
[db]
dsn = "sqlite://`+filepath.Join(dir, "hub.db")+`"
[auth]
token_key_dir = "`+filepath.Join(dir, "keys")+`"
`, 0o600)
	writeFile(t, filepath.Join(dir, "node.toml"), `schema_version = 1
[node]
listen = "`+freePort(t)+`"
[devices.hf]
name = "HF"
type = "rtl_sdr"
freq_range = { min = "100kHz", max = 30_000_000 }
sample_rates = [2_048_000]
`, 0o600)

	return &allEnv{dir: dir, gateway: gw}
}

// start loads the config like `meshsdr all`, creates the CA on first start
// and runs the all role until the returned stop is called.
func (a *allEnv) start(t *testing.T) (*AllProcess, func()) {
	t.Helper()

	opts := config.Options{Dir: a.dir, Env: map[string]string{}}
	pre := opts
	pre.DeferSecrets = true

	preHub, _, _, _, err := config.LoadAll(pre)
	if err != nil {
		t.Fatal(err)
	}

	keyPath, _ := preHub.TLS.CAKey.FilePath(a.dir)

	if _, err := EnsureCA(preHub.TLS.CACert, keyPath, time.Now()); err != nil {
		t.Fatal(err)
	}

	hubCfg, hubMeta, nodeCfg, _, err := config.LoadAll(opts)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	adapter, err := OpenDB(ctx, hubCfg.DB, quiet)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := adapter.Migrator().Up(ctx); err != nil {
		t.Fatal(err)
	}

	p, err := All(ctx, hubCfg, hubMeta.Origins, nodeCfg, quiet, adapter)
	if err != nil {
		t.Fatal(err)
	}

	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)

	go func() { done <- p.Run(rctx) }()

	stopped := false
	stop := func() {
		if stopped {
			return
		}

		stopped = true

		cancel()

		if err := <-done; err != nil {
			t.Errorf("all: %v", err)
		}

		_ = adapter.Close()
	}

	t.Cleanup(stop)

	return p, stop
}

// TestAllRole: the all role creates the CA, enrolls its local node
// in-process and serves it through the gateway like any node; a restart
// keeps the certificate, and lost node files get a new one (GRID-003,
// GRID-014).
func TestAllRole(t *testing.T) {
	a := newAllEnv(t)
	p, stop := a.start(t)

	ctx := context.Background()
	id := domain.MustNodeID("local")

	eventually(t, "local control channel", 15*time.Second, func() bool { return p.g.manager.Connected(id) })

	for _, f := range []string{"tls/ca.pem", "tls/ca.key", "tls/node.pem", "tls/node.key"} {
		if _, err := os.Stat(filepath.Join(a.dir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}

	n, err := p.g.nodes.Get(ctx, "local")
	if err != nil || n.Origin() != domain.OriginConfig || n.Enrollment() != domain.EnrollmentEnrolled || n.URL().Host() != "127.0.0.1" {
		t.Fatalf("local node = %+v, %v", n, err)
	}

	serial := n.Certificate().Serial()

	// The local node goes through the gateway and the token checks.
	e := &gridEnv{gatewayAddr: a.gateway}
	origin := http.Header{}
	origin.Set("Origin", "http://"+a.gateway)

	eventually(t, "media connection", 10*time.Second, func() bool {
		_, st := e.browserDial(t, "local", origin)

		return st == http.StatusSwitchingProtocols
	})

	stop()

	// A restart keeps the certificate.
	p, stop = a.start(t)
	eventually(t, "local control channel", 15*time.Second, func() bool { return p.g.manager.Connected(id) })

	if n, _ := p.g.nodes.Get(ctx, "local"); n.Certificate().Serial() != serial {
		t.Fatalf("certificate reissued on restart: %s != %s", n.Certificate().Serial(), serial)
	}

	stop()

	// Lost node files: a new certificate, the old one revoked.
	if err := os.Remove(filepath.Join(a.dir, "tls", "node.pem")); err != nil {
		t.Fatal(err)
	}

	p, _ = a.start(t)
	eventually(t, "local control channel", 15*time.Second, func() bool { return p.g.manager.Connected(id) })

	n, _ = p.g.nodes.Get(ctx, "local")
	if n.Certificate().Serial() == serial {
		t.Fatal("certificate not reissued")
	}

	revoked, _ := p.g.revocations.List(ctx, time.Now())

	found := false
	for _, r := range revoked {
		found = found || r.Serial() == serial
	}

	if !found {
		t.Fatal("previous local certificate not revoked")
	}
}
