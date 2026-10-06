package keyring_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/infra/keyring"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func open(t *testing.T, dir string) *keyring.Keyring {
	t.Helper()

	k, err := keyring.Open(keyring.Options{Dir: dir, Rotation: 30 * 24 * time.Hour, TokenTTL: 5 * time.Minute}, t0)
	if err != nil {
		t.Fatal(err)
	}

	return k
}

func kids(j token.JWKS) []string {
	out := []string{}
	for _, k := range j.Keys {
		out = append(out, k.Kid)
	}

	return out
}

func TestOpenCreatesASigningKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	k := open(t, dir)

	priv, kid, err := k.Signer(t0)
	if err != nil || kid == "" {
		t.Fatalf("signer: %v", err)
	}

	raw, err := token.Sign(token.Claims{Issuer: "https://hub", Audience: token.Audience("n1"), Subject: "u", ConnectionID: "c",
		IssuedAt: t0, NotBefore: t0, ExpiresAt: t0.Add(5 * time.Minute), ID: "j"}, priv)
	if err != nil {
		t.Fatal(err)
	}

	jwks, revoked := k.Published(t0)
	set, err := token.NewKeySet(jwks, revoked)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := token.Verify(raw, set, token.Expect{Issuer: "https://hub", Audience: token.Audience("n1"), Now: t0}); err != nil {
		t.Errorf("verify: %v", err)
	}

	fi, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v", fi.Mode().Perm())
	}

	// Reopening loads the same key.
	if _, again, _ := open(t, dir).Signer(t0); again != kid {
		t.Errorf("reopened kid %s, want %s", again, kid)
	}
}

func TestRotation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	k := open(t, dir)
	_, first, _ := k.Signer(t0)

	var changes atomic.Int32

	k.OnChange(func() { changes.Add(1) })

	second, err := k.Rotate(t0)
	if err != nil {
		t.Fatal(err)
	}

	// Published at once, signing only after the lead.
	if got := kids(mustPublished(k, t0)); len(got) != 2 {
		t.Errorf("published = %v", got)
	}

	if _, kid, _ := k.Signer(t0.Add(keyring.PublishLead - time.Second)); kid != first {
		t.Errorf("signer before the lead = %s", kid)
	}

	if _, kid, _ := k.Signer(t0.Add(keyring.PublishLead)); kid != second {
		t.Errorf("signer after the lead = %s", kid)
	}

	// The retired key stays published for TTL + leeway, then goes.
	if err := k.Maintain(t0.Add(keyring.PublishLead + 5*time.Minute + token.Leeway)); err != nil {
		t.Fatal(err)
	}

	if got := kids(mustPublished(k, t0.Add(keyring.PublishLead+5*time.Minute+token.Leeway))); len(got) != 2 {
		t.Errorf("retired key dropped too early: %v", got)
	}

	later := t0.Add(keyring.PublishLead + 10*time.Minute)
	if err := k.Maintain(later); err != nil {
		t.Fatal(err)
	}

	if got := kids(mustPublished(k, later)); len(got) != 1 || got[0] != second {
		t.Errorf("published after retention = %v", got)
	}

	if _, err := os.Stat(filepath.Join(dir, first+".ed25519")); !errors.Is(err, os.ErrNotExist) {
		t.Error("retired key file kept")
	}

	if changes.Load() < 2 {
		t.Errorf("changes = %d", changes.Load())
	}

	// Automatic rotation after the period.
	if err := k.Maintain(t0.Add(31 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	if keys, _ := k.List(t0.Add(31 * 24 * time.Hour)); len(keys) != 2 {
		t.Errorf("no automatic rotation: %+v", keys)
	}
}

func mustPublished(k *keyring.Keyring, now time.Time) token.JWKS {
	j, _ := k.Published(now)

	return j
}

func TestRevoke(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	k := open(t, dir)
	_, first, _ := k.Signer(t0)

	if err := k.Revoke("nope", t0); !errors.Is(err, keyring.ErrUnknownKey) {
		t.Errorf("unknown kid: %v", err)
	}

	if err := k.Revoke(first, t0); err != nil {
		t.Fatal(err)
	}

	_, kid, err := k.Signer(t0)
	if err != nil || kid == first {
		t.Fatalf("signer after revocation = %s, %v", kid, err)
	}

	jwks, revoked := k.Published(t0)
	if len(revoked) != 1 || revoked[0] != first || len(jwks.Keys) != 1 || jwks.Keys[0].Kid != kid {
		t.Errorf("published = %v revoked %v", kids(jwks), revoked)
	}

	// Another process (the CLI) rotates: Maintain picks it up.
	other := open(t, dir)
	if _, err := other.Rotate(t0); err != nil {
		t.Fatal(err)
	}

	if err := k.Maintain(t0); err != nil {
		t.Fatal(err)
	}

	if got := kids(mustPublished(k, t0)); len(got) != 2 {
		t.Errorf("CLI rotation not seen: %v", got)
	}
}

func TestInsecurePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	k := open(t, dir)
	_, kid, _ := k.Signer(t0)

	if err := os.Chmod(filepath.Join(dir, kid+".ed25519"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := keyring.Open(keyring.Options{Dir: dir, TokenTTL: time.Minute}, t0); !errors.Is(err, keyring.ErrInsecure) {
		t.Errorf("world-readable key accepted: %v", err)
	}
}
