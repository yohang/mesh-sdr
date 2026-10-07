package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

const minimalHub = `schema_version = 1

[hub]
url = "https://sdr.example.org"
`

func TestLoadHubOriginsAndPrecedence(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"hub.toml": minimalHub + `
[db]
dsn = "sqlite:///var/lib/meshsdr/a.db"
max_read_connections = 2
`,
		"hub.d/20-b.toml": `schema_version = 1
db.dsn = "sqlite:///var/lib/meshsdr/b.db"
`,
		"hub.d/10-a.toml": `schema_version = 1
[db]
dsn = "sqlite:///var/lib/meshsdr/ignored.db"
max_read_connections = 3
`,
		"hub.d/README": "not toml",
	})

	cfg, meta, err := LoadHub(Options{Dir: dir, Env: map[string]string{
		"MESHSDR_LOG__LEVEL":  "debug",
		"MESHSDR_LOG__FORMAT": "",      // empty: unset, not locked
		"MESHSDR_NODE__ID":    "attic", // another role's key is not an error
		"MESHSDR_CONFIG_DIR":  dir,
		"HOME":                "/root",
	}})
	if err != nil {
		t.Fatalf("LoadHub: %v", err)
	}

	if cfg.DB.DSN != "sqlite:///var/lib/meshsdr/b.db" {
		t.Errorf("db.dsn = %q, want the last drop-in", cfg.DB.DSN)
	}

	if cfg.DB.MaxReadConnections != 3 {
		t.Errorf("db.max_read_connections = %d, want 3 (10-a.toml)", cfg.DB.MaxReadConnections)
	}

	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Errorf("log = %+v", cfg.Log)
	}

	if cfg.Gateway.HTTPSListen != ":443" || cfg.Gateway.TLSMode != TLSModeACME {
		t.Errorf("gateway = %+v, want the defaults", cfg.Gateway)
	}

	wantFiles := []string{"hub.toml", filepath.Join("hub.d", "10-a.toml"), filepath.Join("hub.d", "20-b.toml")}
	if strings.Join(meta.Files, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("files = %v, want %v", meta.Files, wantFiles)
	}

	origins := map[string]string{
		"hub.url":                 "hub.toml:4",
		"gateway.https_listen":    "default",
		"db.dsn":                  filepath.Join("hub.d", "20-b.toml") + ":2",
		"db.max_read_connections": filepath.Join("hub.d", "10-a.toml") + ":4",
		"log.level":               "env:MESHSDR_LOG__LEVEL",
		"log.format":              "default",
	}
	for key, want := range origins {
		if got := meta.Origins.Of(key).String(); got != want {
			t.Errorf("origin of %s = %q, want %q", key, got, want)
		}
	}

	if !meta.Origins.Of("log.level").Locked() || meta.Origins.Of("gateway.https_listen").Locked() {
		t.Error("Locked() mismatch")
	}

	if n := meta.Origins.Locked(); n != 4 {
		t.Errorf("Locked() = %d, want 4", n)
	}
}

// Settings keys lock like bootstrap keys: file or env value, with origin.
func TestLoadHubSettings(t *testing.T) {
	dir := writeFiles(t, map[string]string{"hub.toml": minimalHub})

	cfg, meta, err := LoadHub(Options{Dir: dir, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Settings.UI.ThemeMode != "auto" || meta.Origins.Of("settings.ui.theme_mode").Locked() {
		t.Errorf("default theme_mode = %q (%s)", cfg.Settings.UI.ThemeMode, meta.Origins.Of("settings.ui.theme_mode"))
	}

	dir = writeFiles(t, map[string]string{"hub.toml": minimalHub + "\n[settings.ui]\ntheme_mode = \"dark\"\n"})

	cfg, meta, err = LoadHub(Options{Dir: dir, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	if o := meta.Origins.Of("settings.ui.theme_mode"); cfg.Settings.UI.ThemeMode != "dark" || o.String() != "hub.toml:7" {
		t.Errorf("file theme_mode = %q (%s)", cfg.Settings.UI.ThemeMode, o)
	}

	cfg, meta, err = LoadHub(Options{Dir: dir, Env: map[string]string{"MESHSDR_SETTINGS__UI__THEME_MODE": "light"}})
	if err != nil {
		t.Fatal(err)
	}

	if o := meta.Origins.Of("settings.ui.theme_mode"); cfg.Settings.UI.ThemeMode != "light" || o.String() != "env:MESHSDR_SETTINGS__UI__THEME_MODE" {
		t.Errorf("env theme_mode = %q (%s)", cfg.Settings.UI.ThemeMode, o)
	}
}

func TestLoadNode(t *testing.T) {
	dir := writeFiles(t, map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"attic\"\n"})

	cfg, meta, err := LoadNode(Options{Dir: dir, Env: map[string]string{
		"MESHSDR_NODE__LISTEN": "127.0.0.1:9000",
		"MESHSDR_HUB__URL":     "https://x", // hub key in a node env is not an error
	}})
	if err != nil {
		t.Fatalf("LoadNode: %v", err)
	}

	if cfg.Node.ID != "attic" || cfg.Node.Listen != "127.0.0.1:9000" {
		t.Errorf("node = %+v", cfg.Node)
	}

	if got := meta.Origins.Of("node.id").String(); got != "node.toml:3" {
		t.Errorf("origin = %q", got)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		role    Role
		files   map[string]string
		env     map[string]string
		code    string
		origin  string
		message string
	}{
		{name: "missing file", role: RoleHub, files: map[string]string{}, code: CodeConfigMissing},
		{
			name: "missing schema_version in drop-in", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub, "hub.d/a.toml": "[log]\nlevel = \"debug\"\n"},
			code:  CodeSchemaVersionMissing, origin: filepath.Join("hub.d", "a.toml"),
		},
		{
			name: "unsupported schema_version", role: RoleHub,
			files: map[string]string{"hub.toml": "schema_version = 2\n"},
			code:  CodeSchemaVersionUnsupported,
		},
		{
			name: "unknown key", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "ulr = \"https://x\"\n"},
			code:  CodeUnknownKey, origin: "hub.toml:5", message: `did you mean "hub.url"?`,
		},
		{
			name: "unknown table", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[gatway]\nmode = \"x\"\n"},
			code:  CodeUnknownKey, origin: "hub.toml:5",
		},
		{
			name: "unknown env var", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub},
			env:   map[string]string{"MESHSDR_GATEWAY__HTTP_LISTN": ":80"},
			code:  CodeUnknownEnv, origin: "env:MESHSDR_GATEWAY__HTTP_LISTN", message: "MESHSDR_GATEWAY__HTTP_LISTEN",
		},
		{
			name: "parse error", role: RoleHub,
			files: map[string]string{"hub.toml": "schema_version = 1\n[hub\n"},
			code:  CodeParse, origin: "hub.toml:3",
		},
		{
			name: "type mismatch", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[db]\nmax_read_connections = \"four\"\n"},
			code:  CodeParse,
		},
		{
			name: "hub.url required", role: RoleHub,
			files: map[string]string{"hub.toml": "schema_version = 1\n"},
			code:  CodeRequired,
		},
		{
			name: "insecure hub.url", role: RoleHub,
			files: map[string]string{"hub.toml": "schema_version = 1\n[hub]\nurl = \"http://x\"\n"},
			code:  CodeInvalidValue, origin: "hub.toml:3",
		},
		{
			name: "postgres dsn", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub},
			env:   map[string]string{"MESHSDR_DB__DSN": "postgres://db/x"},
			code:  CodeDBEngineUnsupported, origin: "env:MESHSDR_DB__DSN",
		},
		{
			name: "usage policy too long", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub},
			env:   map[string]string{"MESHSDR_SETTINGS__RECEIVER__USAGE_POLICY_TEXT": strings.Repeat("é", 20001)},
			code:  "invalid_usage_policy", origin: "env:MESHSDR_SETTINGS__RECEIVER__USAGE_POLICY_TEXT",
		},
		{
			name: "usage policy not UTF-8", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub},
			env:   map[string]string{"MESHSDR_SETTINGS__RECEIVER__USAGE_POLICY_TEXT": "rules \xff"},
			code:  "invalid_usage_policy",
		},
		{
			name: "invalid theme mode", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[settings.ui]\ntheme_mode = \"sepia\"\n"},
			code:  CodeInvalidValue, origin: "hub.toml:6",
		},
		{
			name: "read pool size", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[db]\nmax_read_connections = 0\n"},
			code:  CodeInvalidValue,
		},
		{
			name: "bad env int", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub},
			env:   map[string]string{"MESHSDR_DB__MAX_READ_CONNECTIONS": "many"},
			code:  CodeInvalidValue,
		},
		{
			name: "bad listen", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[gateway]\nhttps_listen = \"nope\"\n"},
			code:  CodeInvalidValue, origin: "hub.toml:6",
		},
		{
			name: "gateway sidecar", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[gateway]\nmode = \"sidecar\"\n"},
			code:  CodeInvalidValue, origin: "hub.toml:6", message: "not implemented",
		},
		{
			name: "tls off without a plain listener", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[gateway]\ntls_mode = \"off\"\n"},
			code:  CodeRequired, origin: "default",
		},
		{
			name: "tls files without a certificate", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[gateway]\ntls_mode = \"files\"\n"},
			code:  CodeRequired, origin: "default",
		},
		{
			name: "acme on an IP address", role: RoleHub,
			files: map[string]string{"hub.toml": "schema_version = 1\n[hub]\nurl = \"https://10.0.0.1\"\n"},
			code:  CodeInvalidValue, origin: "default", message: "public DNS name",
		},
		{
			name: "bad listen policy", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[settings]\nlisten_policy = \"everyone\"\n"},
			code:  CodeInvalidValue, origin: "hub.toml:6",
		},
		{
			name: "argon2 memory below floor", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[auth.argon2]\nmemory_kib = 1024\n"},
			code:  CodeInvalidValue, origin: "hub.toml:6",
		},
		{
			name: "argon2 iterations below floor", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub},
			env:   map[string]string{"MESHSDR_AUTH__ARGON2__ITERATIONS": "1"},
			code:  CodeInvalidValue, origin: "env:MESHSDR_AUTH__ARGON2__ITERATIONS",
		},
		{
			name: "argon2 parallelism zero", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[auth.argon2]\nparallelism = 0\n"},
			code:  CodeInvalidValue,
		},
		{
			name: "bad admin network", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[admin]\nallowed_networks = [\"10.0.0.1\"]\n"},
			code:  CodeInvalidValue, origin: "hub.toml:6",
		},
		{
			name: "bad trusted proxy", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub},
			env:   map[string]string{"MESHSDR_HTTP__TRUSTED_PROXIES": "10.0.0.0/8,nope"},
			code:  CodeInvalidValue, origin: "env:MESHSDR_HTTP__TRUSTED_PROXIES",
		},
		{
			name: "bad log level", role: RoleNode,
			files: map[string]string{"node.toml": "schema_version = 1\nnode.id = \"attic\"\nlog.level = \"trace\"\n"},
			code:  CodeInvalidValue, origin: "node.toml:3",
		},
		{
			name: "node.id required", role: RoleNode,
			files: map[string]string{"node.toml": "schema_version = 1\n"},
			code:  CodeRequired,
		},
		{
			name: "invalid node.id", role: RoleNode,
			files: map[string]string{"node.toml": "schema_version = 1\n[node]\nid = \"Attic_1\"\n"},
			code:  "invalid_node_id", origin: "node.toml:3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeFiles(t, tt.files)
			env := tt.env
			if env == nil {
				env = map[string]string{}
			}

			var err error
			if tt.role == RoleHub {
				_, _, err = LoadHub(Options{Dir: dir, Env: env})
			} else {
				_, _, err = LoadNode(Options{Dir: dir, Env: env})
			}

			var cerr *Error
			if !errors.As(err, &cerr) {
				t.Fatalf("err = %v, want *config.Error", err)
			}

			for _, p := range cerr.Problems {
				if p.Code == tt.code &&
					(tt.origin == "" || strings.HasPrefix(p.Origin, tt.origin)) &&
					strings.Contains(p.Message, tt.message) {
					return
				}
			}

			t.Fatalf("no problem with code %q origin %q message %q in: %v", tt.code, tt.origin, tt.message, cerr)
		})
	}
}

// secretConfig is a test config with a secret key.
type secretConfig struct {
	SchemaVersion      int  `toml:"schema_version" env:"-"`
	AllowInlineSecrets bool `toml:"allow_inline_secrets" env:"-"`
	SMTP               struct {
		Password Secret `toml:"password" env:"PASSWORD"`
	} `toml:"smtp" envPrefix:"SMTP__"`
}

func (*secretConfig) validate(Origins) []Problem { return nil }

func TestSecrets(t *testing.T) {
	writeSecret := func(t *testing.T, mode os.FileMode) string {
		t.Helper()

		p := filepath.Join(t.TempDir(), "smtp_password")
		if err := os.WriteFile(p, []byte("s3cret\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}

		return p
	}

	tests := []struct {
		name    string
		toml    func(t *testing.T) string
		env     map[string]string
		want    string
		code    string
		warning bool
	}{
		{
			name: "file reference",
			toml: func(t *testing.T) string { return `smtp.password = { file = "` + writeSecret(t, 0o600) + `" }` },
			want: "s3cret",
		},
		{
			name: "group-readable file is fine",
			toml: func(t *testing.T) string { return `smtp.password = { file = "` + writeSecret(t, 0o640) + `" }` },
			want: "s3cret",
		},
		{
			name: "world-readable file",
			toml: func(t *testing.T) string { return `smtp.password = { file = "` + writeSecret(t, 0o644) + `" }` },
			code: CodeInsecureSecretFile,
		},
		{
			name: "group-writable file",
			toml: func(t *testing.T) string { return `smtp.password = { file = "` + writeSecret(t, 0o660) + `" }` },
			code: CodeInsecureSecretFile,
		},
		{
			name: "missing file",
			toml: func(*testing.T) string { return `smtp.password = { file = "/nonexistent/secret" }` },
			code: CodeSecretUnresolved,
		},
		{
			name: "env reference",
			toml: func(*testing.T) string { return `smtp.password = { env = "SMTP_PASSWORD" }` },
			env:  map[string]string{"SMTP_PASSWORD": "fromenv"},
			want: "fromenv",
		},
		{
			name: "unset env reference",
			toml: func(*testing.T) string { return `smtp.password = { env = "SMTP_PASSWORD" }` },
			code: CodeSecretUnresolved,
		},
		{
			name: "bad reference",
			toml: func(*testing.T) string { return `smtp.password = { path = "x" }` },
			code: CodeParse,
		},
		{
			name: "inline forbidden",
			toml: func(*testing.T) string { return `smtp.password = "inline"` },
			code: CodeInlineSecretForbidden,
		},
		{
			name:    "inline allowed",
			toml:    func(*testing.T) string { return "allow_inline_secrets = true\nsmtp.password = \"inline\"" },
			want:    "inline",
			warning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"hub.toml": "schema_version = 1\n" + tt.toml(t) + "\n"})
			env := tt.env
			if env == nil {
				env = map[string]string{}
			}

			var cfg secretConfig
			meta, err := load(RoleHub, &cfg, Options{Dir: dir, Env: env})

			if tt.code != "" {
				var cerr *Error
				if !errors.As(err, &cerr) || cerr.Problems[0].Code != tt.code {
					t.Fatalf("err = %v, want code %s", err, tt.code)
				}

				if strings.Contains(err.Error(), "s3cret") {
					t.Fatal("error leaks the secret")
				}

				return
			}

			if err != nil {
				t.Fatalf("load: %v", err)
			}

			if got := cfg.SMTP.Password.Reveal(); got != tt.want {
				t.Errorf("Reveal() = %q, want %q", got, tt.want)
			}

			if s := cfg.SMTP.Password.String(); s != "[redacted]" {
				t.Errorf("String() = %q", s)
			}

			if tt.warning != (len(meta.Warnings) > 0) {
				t.Errorf("warnings = %v", meta.Warnings)
			}
		})
	}
}

// Every key maps to its env var: db.dsn → DB__DSN.
func TestEnvNamesFollowKeys(t *testing.T) {
	for _, v := range []any{&Hub{}, &Node{}} {
		for _, lf := range leaves(v) {
			want := strings.ToUpper(strings.ReplaceAll(lf.key, ".", "__"))
			if lf.key == "schema_version" || lf.key == "allow_inline_secrets" || lf.value.Kind() == reflect.Map {
				want = "" // map-of-tables keys are file-only (ADR 0008)
			}

			if lf.env != want {
				t.Errorf("%s: env = %q, want %q", lf.key, lf.env, want)
			}
		}
	}
}

func TestAuthDefaultsAndEnvLists(t *testing.T) {
	dir := writeFiles(t, map[string]string{"hub.toml": minimalHub})

	cfg, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{
		"MESHSDR_HTTP__TRUSTED_PROXIES": "10.0.0.0/8,::ffff:192.168.1.0/120",
	}})
	if err != nil {
		t.Fatal(err)
	}

	if a := cfg.Auth.Argon2; a.MemoryKiB != 65536 || a.Iterations != 3 || a.Parallelism != 1 {
		t.Errorf("argon2 defaults = %+v", a)
	}

	if got := cfg.Admin.AllowedNetworks; len(got) != 2 || got[0] != "0.0.0.0/0" || got[1] != "::/0" {
		t.Errorf("admin.allowed_networks = %v", got)
	}

	got := Prefixes(cfg.HTTP.TrustedProxies)
	if len(got) != 2 || got[0].String() != "10.0.0.0/8" || got[1].String() != "192.168.1.0/24" {
		t.Errorf("trusted proxies = %v", got)
	}
}

func TestSMTP(t *testing.T) {
	load := func(extra string, env map[string]string) (Hub, error) {
		dir := writeFiles(t, map[string]string{"hub.toml": minimalHub + extra})
		if env == nil {
			env = map[string]string{}
		}

		cfg, _, err := LoadHub(Options{Dir: dir, Env: env})

		return cfg, err
	}

	cfg, err := load("", nil)
	if err != nil || cfg.SMTP.Enabled() || cfg.SMTP.Port != 587 || cfg.SMTP.TLS != "starttls" {
		t.Fatalf("defaults = %+v, %v", cfg.SMTP, err)
	}

	cfg, err = load("[smtp]\nhost = \"smtp.example.org\"\nfrom = \"WebSDR <sdr@example.org>\"\n",
		map[string]string{"MESHSDR_SMTP__PASSWORD": "pw", "MESHSDR_SMTP__USERNAME": "sdr"})
	if err != nil || !cfg.SMTP.Enabled() || cfg.SMTP.Password.Reveal() != "pw" {
		t.Fatalf("configured = %+v, %v", cfg.SMTP, err)
	}

	for name, extra := range map[string]string{
		"no from":     "[smtp]\nhost = \"smtp.example.org\"\n",
		"bad from":    "[smtp]\nhost = \"smtp.example.org\"\nfrom = \"not an address\"\n",
		"plain text":  "[smtp]\nhost = \"smtp.example.org\"\nfrom = \"a@b.example\"\ntls = \"none\"\n",
		"unknown tls": "[smtp]\nhost = \"smtp.example.org\"\nfrom = \"a@b.example\"\ntls = \"ssl\"\n",
		"bad port":    "[smtp]\nhost = \"smtp.example.org\"\nfrom = \"a@b.example\"\nport = 0\n",
		"bad host":    "[smtp]\nhost = \"smtp example\"\nfrom = \"a@b.example\"\n",
	} {
		if _, err := load(extra, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	if _, err := load("[smtp]\nhost = \"mailpit\"\nport = 1025\nfrom = \"a@b.example\"\ntls = \"none\"\nallow_insecure = true\n", nil); err != nil {
		t.Errorf("explicitly insecure relay refused: %v", err)
	}
}

// The operator key of the public listener must be a 0600 file.
func TestGatewayKeyFileMode(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"hub.toml":    minimalHub + "[gateway]\ntls_mode = \"files\"\ntls_cert = \"tls/pub.pem\"\ntls_key = \"tls/pub.key\"\n",
		"tls/pub.pem": "cert", "tls/pub.key": "key",
	})

	if _, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{}}); err != nil {
		t.Fatalf("0600 key: %v", err)
	}

	if err := os.Chmod(filepath.Join(dir, "tls", "pub.key"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{}})

	var cerr *Error
	if !errors.As(err, &cerr) || cerr.Problems[0].Code != CodeInsecureSecretFile || cerr.Problems[0].Key != "gateway.tls_key" {
		t.Fatalf("world-readable key: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "tls", "pub.key")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{}}); !errors.As(err, &cerr) || cerr.Problems[0].Key != "gateway.tls_key" {
		t.Fatalf("missing key: %v", err)
	}
}
