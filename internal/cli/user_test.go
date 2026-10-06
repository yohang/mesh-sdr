package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

func runIn(t *testing.T, ctx context.Context, env map[string]string, stdin string, args ...string) result {
	t.Helper()

	var stdout, stderr bytes.Buffer

	a := &app{stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr, env: env}
	code := a.execute(ctx, args)

	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestUserCommands(t *testing.T) {
	ctx := context.Background()
	env := map[string]string{"MESHSDR_CONFIG_DIR": hubDir(t)}

	if r := run(t, ctx, env, "hub", "user", "exists", "alice"); r.code != ExitFailure || !strings.Contains(r.stderr, "migrations_pending") {
		t.Fatalf("user command on a pending schema = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "migrate"); r.code != ExitOK {
		t.Fatalf("migrate = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "user", "exists", "alice"); r.code != ExitFailure || strings.Contains(r.stderr, "meshsdr:") || r.stdout != "" {
		t.Errorf("exists (missing) = %+v", r)
	}

	r := runIn(t, ctx, env, "a long password\na long password\n", "hub", "user", "add", "alice", "--role", "admin", "--email", "alice@example.org")
	if r.code != ExitOK || !strings.Contains(r.stdout, "user alice created (role admin)") || strings.Contains(r.stdout, "password:") {
		t.Fatalf("add (interactive) = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "user", "exists", "ALICE"); r.code != ExitOK {
		t.Errorf("exists = %+v", r)
	}

	if r := runIn(t, ctx, env, "a long password\nanother one!!\n", "hub", "user", "add", "bob"); r.code != ExitFailure || !strings.Contains(r.stderr, "do not match") {
		t.Errorf("add with mismatched passwords = %+v", r)
	}

	if r := runIn(t, ctx, env, "a long password\na long password\n", "hub", "user", "add", "alice"); r.code != ExitFailure || !strings.Contains(r.stderr, "username_taken") {
		t.Errorf("duplicate add = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "user", "add", "carol", "--role", "root"); r.code != ExitFailure || !strings.Contains(r.stderr, "--role") {
		t.Errorf("bad role = %+v", r)
	}

	withPw := maps.Clone(env)
	withPw["MESHSDR_PASSWORD"] = "from the environment"

	if r := run(t, ctx, withPw, "--noninteractive", "hub", "user", "add", "dave", "--role", "admin"); r.code != ExitOK || strings.Contains(r.stdout, "password:") {
		t.Errorf("add --noninteractive with MESHSDR_PASSWORD = %+v", r)
	}

	if r := runIn(t, ctx, withPw, "short\nshort\n", "hub", "user", "add", "erin"); r.code != ExitFailure || !strings.Contains(r.stderr, "invalid_password") {
		t.Errorf("short password = %+v", r)
	}

	r = run(t, ctx, env, "--noninteractive", "--json", "hub", "user", "add", "frank", "--role", "operator")

	var added struct {
		ID                 string `json:"id"`
		Role               string `json:"role"`
		MustChangePassword bool   `json:"must_change_password"`
		Password           string `json:"password"`
	}
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &added) != nil || added.Role != "operator" ||
		!added.MustChangePassword || len(added.Password) < 20 || added.ID == "" {
		t.Errorf("add --json with a generated password = %+v (%+v)", r, added)
	}

	if r := run(t, ctx, env, "--noninteractive", "--silent", "hub", "user", "add", "gina"); r.code != ExitOK ||
		!strings.HasPrefix(r.stdout, "password: ") {
		t.Errorf("generated password hidden by --silent: %+v", r)
	}

	if r := run(t, ctx, env, "hub", "user", "disable", "alice"); r.code != ExitOK || !strings.Contains(r.stdout, "user alice disabled, 0 session(s) revoked") {
		t.Errorf("disable = %+v", r)
	}

	if r := run(t, ctx, env, "--json", "hub", "user", "disable", "alice"); r.code != ExitOK || !strings.Contains(r.stdout, `"changed": false`) {
		t.Errorf("disable again = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "user", "enable", "alice"); r.code != ExitOK || !strings.Contains(r.stdout, "user alice enabled") {
		t.Errorf("enable = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "user", "enable", "alice"); r.code != ExitOK || !strings.Contains(r.stdout, "already enabled") {
		t.Errorf("enable again = %+v", r)
	}

	if r := runIn(t, ctx, env, "a new long password\na new long password\n", "hub", "user", "reset-password", "alice"); r.code != ExitOK ||
		!strings.Contains(r.stdout, "password of alice reset, 0 session(s) revoked") || strings.Contains(r.stdout, "password:") {
		t.Errorf("reset-password (interactive) = %+v", r)
	}

	r = run(t, ctx, env, "--noninteractive", "--json", "hub", "user", "reset-password", "alice")

	var reset struct {
		MustChangePassword bool   `json:"must_change_password"`
		Password           string `json:"password"`
	}
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &reset) != nil || !reset.MustChangePassword || len(reset.Password) < 20 {
		t.Errorf("reset-password (generated) = %+v", r)
	}

	if r := run(t, ctx, withPw, "--noninteractive", "hub", "user", "reset-password", "alice"); r.code != ExitOK || strings.Contains(r.stdout, "password:") {
		t.Errorf("reset-password with MESHSDR_PASSWORD = %+v", r)
	}

	if r := runIn(t, ctx, env, "password123\npassword123\n", "hub", "user", "reset-password", "alice"); r.code != ExitFailure || !strings.Contains(r.stderr, "invalid_password") {
		t.Errorf("reset-password with a common password = %+v", r)
	}

	if r := run(t, ctx, env, "hub", "user", "disable", "dave"); r.code != ExitOK {
		t.Fatalf("disable dave = %+v", r)
	}

	r = run(t, ctx, env, "hub", "user", "list")
	if lines := strings.Split(strings.TrimSpace(r.stdout), "\n"); r.code != ExitOK || len(lines) != 4 ||
		!strings.HasPrefix(lines[0], "USERNAME") || !strings.Contains(lines[1], "alice@example.org") ||
		!strings.Contains(lines[1], "listener,admin") || !strings.Contains(lines[1], "never") ||
		strings.Contains(r.stdout, "dave") || !strings.Contains(r.stdout, "listener,operator") {
		t.Errorf("list = %+v", r)
	}

	r = run(t, ctx, env, "--json", "hub", "user", "list", "--all")

	var listed []struct {
		Username string   `json:"username"`
		Email    *string  `json:"email"`
		Roles    []string `json:"roles"`
		Enabled  bool     `json:"enabled"`
	}
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &listed) != nil || len(listed) != 4 ||
		listed[0].Username != "alice" || listed[0].Email == nil || listed[1].Username != "dave" || listed[1].Enabled ||
		listed[1].Email != nil || len(listed[0].Roles) != 2 {
		t.Errorf("list --all --json = %+v (%+v)", r, listed)
	}

	for _, cmd := range []string{"disable", "enable", "reset-password"} {
		r := run(t, ctx, env, "--json", "--noninteractive", "hub", "user", cmd, "nobody")
		if r.code != ExitFailure || !strings.Contains(r.stderr, `"code":"user_not_found"`) {
			t.Errorf("%s nobody = %+v", cmd, r)
		}
	}

	if r := run(t, ctx, env, "--json", "hub", "user", "exists", "nobody"); r.code != ExitFailure || !strings.Contains(r.stdout, `"exists": false`) {
		t.Errorf("exists --json = %+v", r)
	}
}

// AUTH-015: help without a subcommand, one-line errors with a non-zero code.
func TestGlobalBehaviour(t *testing.T) {
	ctx := context.Background()

	r := run(t, ctx, nil)
	if r.code != ExitOK || !strings.Contains(r.stdout, "Usage:") || !strings.Contains(r.stdout, "--noninteractive") ||
		!strings.Contains(r.stdout, "--config-dir") {
		t.Errorf("no subcommand = %+v", r)
	}

	r = run(t, ctx, map[string]string{"MESHSDR_CONFIG_DIR": t.TempDir()}, "hub", "user", "exists", "x")
	if r.code == ExitOK || strings.Count(strings.TrimSpace(r.stderr), "\n") != 0 || !strings.HasPrefix(r.stderr, "meshsdr: ") {
		t.Errorf("error output = %+v", r)
	}
}
