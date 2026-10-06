package config

import (
	"errors"
	"os"
	"path/filepath"
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

	if cfg.Hub.Listen != "0.0.0.0:8073" {
		t.Errorf("hub.listen = %q, want default", cfg.Hub.Listen)
	}

	wantFiles := []string{"hub.toml", filepath.Join("hub.d", "10-a.toml"), filepath.Join("hub.d", "20-b.toml")}
	if strings.Join(meta.Files, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("files = %v, want %v", meta.Files, wantFiles)
	}

	origins := map[string]string{
		"hub.url":                 "hub.toml:4",
		"hub.listen":              "default",
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

	if !meta.Origins.Of("log.level").Locked() || meta.Origins.Of("hub.listen").Locked() {
		t.Error("Locked() mismatch")
	}

	if n := meta.Origins.Locked(); n != 4 {
		t.Errorf("Locked() = %d, want 4", n)
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
			files: map[string]string{"hub.toml": minimalHub + "lisen = \":80\"\n"},
			code:  CodeUnknownKey, origin: "hub.toml:5", message: `did you mean "hub.listen"?`,
		},
		{
			name: "unknown table", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub + "[gatway]\nmode = \"x\"\n"},
			code:  CodeUnknownKey, origin: "hub.toml:5",
		},
		{
			name: "unknown env var", role: RoleHub,
			files: map[string]string{"hub.toml": minimalHub},
			env:   map[string]string{"MESHSDR_HUB__LISTN": ":80"},
			code:  CodeUnknownEnv, origin: "env:MESHSDR_HUB__LISTN", message: "MESHSDR_HUB__LISTEN",
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
			files: map[string]string{"hub.toml": minimalHub + "listen = \"nope\"\n"},
			code:  CodeInvalidValue, origin: "hub.toml:5",
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
			if lf.key == "schema_version" || lf.key == "allow_inline_secrets" {
				want = ""
			}

			if lf.env != want {
				t.Errorf("%s: env = %q, want %q", lf.key, lf.env, want)
			}
		}
	}
}
