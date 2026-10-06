package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHubNodeCommands(t *testing.T) {
	ctx := context.Background()
	dir := hubDir(t)
	env := map[string]string{"MESHSDR_CONFIG_DIR": dir}

	if r := run(t, ctx, env, "hub", "migrate"); r.code != ExitOK {
		t.Fatalf("migrate = %+v", r)
	}

	// Without the hub CA, tokens cannot be issued.
	if r := run(t, ctx, env, "hub", "node", "add", "attic", "--url", "https://10.0.0.1:8074"); r.code != ExitFailure || !strings.Contains(r.stderr, "ca init") {
		t.Fatalf("add without CA = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "ca", "init"); r.code != ExitOK {
		t.Fatalf("ca init = %+v", r)
	}

	appendFile(t, dir, "hub.toml", "\n[tls]\nca_cert = \"tls/ca.pem\"\nca_key = { file = \"tls/ca.key\" }\n")

	r := run(t, ctx, env, "--json", "hub", "node", "add", "attic", "--url", "https://10.0.0.1:8074")

	var issued issuedJSON
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &issued) != nil || issued.EnrollmentToken == "" || issued.Node.EnrollmentState != "pending" {
		t.Fatalf("add = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "node", "disable", "attic"); r.code != ExitOK {
		t.Fatalf("disable = %+v", r)
	}

	r = run(t, ctx, env, "--json", "hub", "node", "list")

	var list []nodeJSON
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &list) != nil || len(list) != 1 || !list[0].Disabled {
		t.Fatalf("list = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "node", "token", "attic"); r.code != ExitOK || !strings.Contains(r.stdout, "enrollment token") {
		t.Fatalf("token = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "node", "remove", "attic"); r.code != ExitOK {
		t.Fatalf("remove = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "node", "show", "attic"); r.code != ExitFailure || !strings.Contains(r.stderr, "not found") {
		t.Fatalf("show removed = %+v", r)
	}
}
