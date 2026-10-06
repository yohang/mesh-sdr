package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, ctx context.Context, env map[string]string, args ...string) result {
	t.Helper()

	var stdout, stderr bytes.Buffer

	a := &app{stdout: &stdout, stderr: &stderr, env: env}
	code := a.execute(ctx, args)

	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func hubDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	cfg := `schema_version = 1

[hub]
listen = "127.0.0.1:0"
url = "http://localhost"
allow_insecure_url = true

[db]
dsn = "sqlite://` + filepath.Join(dir, "hub.db") + `"

[log]
format = "text"
`

	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestHubLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := hubDir(t)
	env := map[string]string{"MESHSDR_CONFIG_DIR": dir}

	r := run(t, ctx, env, "hub")
	if r.code != ExitFailure || !strings.Contains(r.stderr, "migrations_pending") || !strings.Contains(r.stderr, "meshsdr hub migrate") {
		t.Fatalf("hub with pending migrations = %+v", r)
	}

	r = run(t, ctx, env, "--json", "hub")
	if r.code != ExitFailure || !strings.Contains(r.stderr, `"code":"migrations_pending"`) {
		t.Fatalf("hub --json = %+v", r)
	}

	r = run(t, ctx, nil, "-c", dir, "hub", "migrate", "status")
	if r.code != ExitFailure || !strings.Contains(r.stdout, "00001_init.sql") || !strings.Contains(r.stdout, "schema: migrations_pending") {
		t.Fatalf("migrate status on a fresh database = %+v", r)
	}

	r = run(t, ctx, env, "--json", "hub", "migrate", "status")

	var status struct {
		Migrations []struct {
			Name    string `json:"name"`
			Applied bool   `json:"applied"`
		} `json:"migrations"`
		Schema struct {
			OK   bool   `json:"ok"`
			Code string `json:"code"`
		} `json:"schema"`
	}
	if r.code != ExitFailure || json.Unmarshal([]byte(r.stdout), &status) != nil ||
		status.Schema.OK || status.Schema.Code != "migrations_pending" || len(status.Migrations) < 2 || status.Migrations[0].Applied {
		t.Fatalf("migrate status --json = %+v (%+v)", r, status)
	}

	r = run(t, ctx, env, "hub", "migrate")
	if r.code != ExitOK || !strings.Contains(r.stdout, "applied 00001_init.sql") {
		t.Fatalf("migrate = %+v", r)
	}

	r = run(t, ctx, env, "--json", "hub", "migrate", "up")

	var applied []map[string]any
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &applied) != nil || len(applied) != 0 {
		t.Fatalf("second migrate up = %+v", r)
	}

	r = run(t, ctx, env, "hub", "migrate", "status")
	if r.code != ExitOK || !strings.Contains(r.stdout, "schema: up to date") || !strings.Contains(r.stdout, "applied 20") {
		t.Fatalf("migrate status = %+v", r)
	}

	r = run(t, ctx, env, "--json", "hub", "migrate", "status")
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &status) != nil || !status.Schema.OK || !status.Migrations[0].Applied {
		t.Fatalf("migrate status --json = %+v (%+v)", r, status)
	}

	// The hub now starts, and stops gracefully when its context ends.
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	r = run(t, runCtx, env, "hub")
	if r.code != ExitOK || !strings.Contains(r.stderr, "http server listening") || !strings.Contains(r.stderr, "hub stopped") {
		t.Fatalf("hub = %+v", r)
	}

	r = run(t, ctx, env, "--silent", "hub", "migrate", "down")
	if r.code != ExitOK || r.stdout != "" {
		t.Fatalf("migrate down --silent = %+v", r)
	}
}

func TestNodeLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node.toml"), []byte("schema_version = 1\n[node]\nid = \"attic\"\nlisten = \"127.0.0.1:0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	r := run(t, ctx, map[string]string{"MESHSDR_LOG__FORMAT": "text"}, "--json", "-c", dir, "node")
	if r.code != ExitOK || !strings.Contains(r.stderr, `"msg":"node starting: not enrolled, serving the pre-enrollment API only`) {
		t.Fatalf("node = %+v", r)
	}
}

func TestInvalidConfigExitsWithEXCONFIG(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte("schema_version = 1\nbogus = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"hub"}, {"hub", "migrate"}, {"hub", "config", "check"}} {
		r := run(t, context.Background(), map[string]string{}, append([]string{"-c", dir}, args...)...)
		if r.code != ExitConfig || !strings.Contains(r.stderr, "hub.toml:2: bogus: unknown key") {
			t.Errorf("%v = %+v", args, r)
		}
	}

	r := run(t, context.Background(), map[string]string{}, "-c", filepath.Join(dir, "missing"), "node")
	if r.code != ExitConfig || !strings.Contains(r.stderr, "config_missing") {
		t.Errorf("missing node.toml = %+v", r)
	}
}

func TestConfigCommands(t *testing.T) {
	dir := hubDir(t)
	env := map[string]string{"MESHSDR_LOG__LEVEL": "warn"}

	r := run(t, context.Background(), env, "-c", dir, "--json", "hub", "config", "check")

	var out struct {
		Valid   bool              `json:"valid"`
		Origins map[string]string `json:"origins"`
	}
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &out) != nil || !out.Valid ||
		out.Origins["log.level"] != "env:MESHSDR_LOG__LEVEL" || out.Origins["hub.url"] != "hub.toml:5" {
		t.Fatalf("config check = %+v (%+v)", r, out)
	}

	for _, role := range []string{"hub", "node"} {
		r := run(t, context.Background(), env, role, "config", "schema")

		var s map[string]any
		if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &s) != nil || s["$id"] != "urn:meshsdr:config:"+role {
			t.Errorf("%s config schema = %+v", role, r)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	r := run(t, context.Background(), map[string]string{}, "serve")
	if r.code != ExitFailure || !strings.Contains(r.stderr, "unknown command") {
		t.Fatalf("serve = %+v", r)
	}
}
