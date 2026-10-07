package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestGridTLSKeys(t *testing.T) {
	fp := "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"

	dir := writeFiles(t, map[string]string{"node.toml": `schema_version = 1
[node]
id = "attic"
[tls]
cert = "tls/node.pem"
key = "/abs/node.key"
[hub_trust]
ca_cert = "tls/ca.pem"
ca_fingerprint = "` + fp + `"
hub_identity = "hub.example.org"
`})

	cfg, _, err := LoadNode(Options{Dir: dir, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.TLS.Cert != filepath.Join(dir, "tls/node.pem") || cfg.TLS.Key != "/abs/node.key" || cfg.HubTrust.CACert != filepath.Join(dir, "tls/ca.pem") {
		t.Errorf("paths not resolved against the config dir: %+v %+v", cfg.TLS, cfg.HubTrust)
	}

	hubDir := writeFiles(t, map[string]string{
		"hub.toml": minimalHub + `
[nodes.attic]
url = "https://10.0.0.1:8074"
enrollment_token = { file = "attic.token" }

[nodes.garden]
url = "https://garden:8074"
name = "Garden"
`,
		"attic.token": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8\n",
	})

	hub, meta, err := LoadHub(Options{Dir: hubDir, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	if len(hub.Nodes) != 2 || hub.Nodes["attic"].EnrollmentToken.Reveal() != "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" || hub.Nodes["garden"].Name != "Garden" {
		t.Errorf("nodes = %+v", hub.Nodes)
	}

	if got := meta.Origins.Of("nodes.garden.name").String(); got != "hub.toml" {
		t.Errorf("origin of nodes.garden.name = %q", got)
	}

	devDir := writeFiles(t, map[string]string{"node.toml": `schema_version = 1
[node]
id = "attic"
[devices.hf-sdrplay]
name = "SDRplay RSPdx (HF)"
type = "soapy:sdrplay"
enabled = true
listen_policy = "registered"
operator_can_retune = true
freq_range = { min = 1_000, max = "30MHz" }
sample_rates = [500_000, 2_000_000]
[devices.vhf]
name = "VHF"
type = "rtl_sdr"
freq_range = { min = "24MHz", max = "1.766GHz" }
sample_rates = [2_048_000]
`})

	node, _, err := LoadNode(Options{Dir: devDir, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	if d := node.Devices["hf-sdrplay"]; d.FreqRange.Max.Hz() != 30_000_000 || len(d.SampleRates) != 2 || !d.OperatorCanRetune || node.Devices["vhf"].Enabled != nil {
		t.Errorf("devices = %+v", node.Devices)
	}

	tests := []struct {
		name  string
		role  Role
		files map[string]string
		key   string
	}{
		{"ca_cert without ca_key", RoleHub, map[string]string{"hub.toml": minimalHub + "[tls]\nca_cert = \"ca.pem\"\n"}, "tls.ca_key"},
		{"node url not https", RoleHub, map[string]string{"hub.toml": minimalHub + "[nodes.attic]\nurl = \"http://x:1\"\n"}, "nodes.attic.url"},
		{"inline node token", RoleHub, map[string]string{"hub.toml": minimalHub + "[nodes.attic]\nurl = \"https://x:1\"\nenrollment_token = \"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8\"\n"}, "nodes.attic.enrollment_token"},
		{"low-entropy node token", RoleHub, map[string]string{
			"hub.toml": minimalHub + "[nodes.attic]\nurl = \"https://x:1\"\nenrollment_token = { file = \"t\" }\n",
			"t":        "abcdefghijklmnopqrstuvwxyz\n",
		}, "nodes.attic.enrollment_token"},
		{"bad node id", RoleHub, map[string]string{"hub.toml": minimalHub + "[nodes.A]\nurl = \"https://x:1\"\n"}, "nodes.A"},
		{"device range", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\n[devices.hf]\nname = \"x\"\ntype = \"rtl_sdr\"\nfreq_range = { min = 10, max = 5 }\nsample_rates = [1]\n"}, "devices.hf.freq_range"},
		{"cert without key", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\n[tls]\ncert = \"a\"\n"}, "tls.key"},
		{"bad fingerprint", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\n[hub_trust]\nca_fingerprint = \"zz\"\n"}, "hub_trust.ca_fingerprint"},
		{"relative runtime dir", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\nruntime_dir = \"run\"\n"}, "node.runtime_dir"},
		{"bad port range", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\nipc_port_range = \"5000-4000\"\n"}, "node.ipc_port_range"},
		{"tiny notsent lowat", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\nws_notsent_lowat = 10\n"}, "node.ws_notsent_lowat"},
		{"relative tool dir", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\n[tools]\ndirs = [\"bin\"]\n"}, "tools.dirs[0]"},
		{"relative tool", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\n[tools]\nrtl_connector = \"rtl_connector\"\n"}, "tools.rtl_connector"},
		{"rtl_tcp without port", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\n[devices.v]\nname = \"x\"\nfreq_range = { min = 10, max = 50 }\nsample_rates = [1]\ntype = \"rtl_tcp\"\ndriver = { device = \"sdr.lan\" }\n"}, "devices.v.driver"},
		{"gain out of range", RoleNode, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\n[devices.v]\nname = \"x\"\nfreq_range = { min = 10, max = 50 }\nsample_rates = [1]\ntype = \"rtl_sdr\"\ndriver = { rf_gain = 300 }\n"}, "devices.v.driver.rf_gain"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeFiles(t, tt.files)

			var err error
			if tt.role == RoleHub {
				_, _, err = LoadHub(Options{Dir: dir, Env: map[string]string{}})
			} else {
				_, _, err = LoadNode(Options{Dir: dir, Env: map[string]string{}})
			}

			var cerr *Error
			if !errors.As(err, &cerr) || len(cerr.Problems) != 1 || cerr.Problems[0].Key != tt.key {
				t.Fatalf("err = %v, want one problem on %s", err, tt.key)
			}
		})
	}
}
