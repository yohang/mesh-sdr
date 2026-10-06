package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCAInit(t *testing.T) {
	dir := hubDir(t)
	env := map[string]string{}

	r := run(t, context.Background(), env, "-c", dir, "--json", "hub", "ca", "init")

	var out caInitJSON
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &out) != nil || len(out.Fingerprint) != 95 {
		t.Fatalf("ca init = %+v (%+v)", r, out)
	}

	info, err := os.Stat(filepath.Join(dir, "tls", "ca.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key: %v %v", info, err)
	}

	if r := run(t, context.Background(), env, "-c", dir, "hub", "ca", "init"); r.code != ExitFailure || !strings.Contains(r.stderr, "refusing to overwrite") {
		t.Fatalf("second ca init = %+v", r)
	}

	// The hub config accepts the CA files with paths relative to the config dir.
	f, err := os.OpenFile(filepath.Join(dir, "hub.toml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = f.WriteString("\n[tls]\nca_cert = \"tls/ca.pem\"\nca_key = { file = \"tls/ca.key\" }\n")
	_ = f.Close()

	if r := run(t, context.Background(), env, "-c", dir, "hub", "config", "check"); r.code != ExitOK {
		t.Fatalf("config check = %+v", r)
	}
}
