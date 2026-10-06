package cli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

func freeAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()
	_ = ln.Close()

	return addr
}

func TestNodeEnroll(t *testing.T) {
	certPEM, keyPEM, _ := pki.GenerateCA("hub", time.Now())
	ca, _ := pki.ParseCA(certPEM, keyPEM)
	tok, _ := domain.NewEnrollmentToken()
	addr := freeAddr(t)

	dir := t.TempDir()
	cfg := "schema_version = 1\n[node]\nid = \"attic\"\nlisten = \"" + addr + "\"\n[tls]\ncert = \"tls/node.pem\"\nkey = \"tls/node.key\"\n" +
		"[hub_trust]\nca_cert = \"tls/ca.pem\"\nca_fingerprint = \"" + pki.FormatFingerprint(ca.Fingerprint()) + "\"\n[log]\nformat = \"text\"\n"

	if err := os.WriteFile(filepath.Join(dir, "node.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(tok.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without a token the command refuses to start.
	if r := run(t, context.Background(), map[string]string{}, "-c", dir, "node", "enroll"); r.code != ExitFailure || !strings.Contains(r.stderr, "token is required") {
		t.Fatalf("enroll without token = %+v", r)
	}

	done := make(chan result, 1)

	go func() {
		done <- run(t, context.Background(), map[string]string{}, "-c", dir, "node", "enroll", "--token-file", tokenFile, "--timeout", "20s")
	}()

	hub := enroll.NewHubClient(ca, time.Now)
	target := gridapp.EnrollmentTarget{ID: domain.MustNodeID("attic"), URL: domain.MustNodeURL("https://" + addr), Key: tok.Key()}

	var err error

	for range 100 {
		if _, err = hub.Enroll(context.Background(), target); err == nil {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	if err != nil {
		t.Fatalf("hub enroll: %v", err)
	}

	r := <-done
	if r.code != ExitOK {
		t.Fatalf("node enroll = %+v", r)
	}

	if _, err := pki.LoadKeyPair(filepath.Join(dir, "tls", "node.pem"), filepath.Join(dir, "tls", "node.key")); err != nil {
		t.Fatal(err)
	}

	// A second run refuses to overwrite the enrolled identity.
	if r := run(t, context.Background(), map[string]string{}, "-c", dir, "node", "enroll", "--token-file", tokenFile); r.code != ExitFailure || !strings.Contains(r.stderr, "already enrolled") {
		t.Fatalf("second enroll = %+v", r)
	}
}
