package domain_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func TestNodeURL(t *testing.T) {
	ok := map[string]string{
		"https://10.0.0.1:8074":      "https://10.0.0.1:8074",
		"https://node.example:8074/": "https://node.example:8074",
		"https://[2001:db8::1]:8074": "https://[2001:db8::1]:8074",
	}
	for in, want := range ok {
		u, err := domain.NewNodeURL(in)
		if err != nil || u.String() != want {
			t.Errorf("NewNodeURL(%q) = %q, %v", in, u, err)
		}
	}

	for _, in := range []string{"http://x:1", "https://x", "https://x:0", "https://x:1/path", "https://u@x:1", "x:1", ""} {
		if _, err := domain.NewNodeURL(in); !errors.Is(err, domain.ErrInvalidNodeURL) {
			t.Errorf("NewNodeURL(%q) = %v, want invalid", in, err)
		}
	}

	if got := domain.MustNodeURL("https://x:1").Endpoint("wss", "/control"); got != "wss://x:1/control" {
		t.Errorf("endpoint = %q", got)
	}
}

func TestNodeName(t *testing.T) {
	for _, in := range []string{"", "  ", "a\x00b", string(make([]byte, 129))} {
		if _, err := domain.NewNodeName(in); !errors.Is(err, domain.ErrInvalidNodeName) {
			t.Errorf("NewNodeName(%q) = %v", in, err)
		}
	}
}

func TestEnrollmentToken(t *testing.T) {
	tok, err := domain.NewEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}

	if len(tok.String()) != 43 {
		t.Errorf("token %q", tok)
	}

	parsed, err := domain.ParseEnrollmentToken(tok.String())
	if err != nil || parsed.Key() != tok.Key() {
		t.Fatalf("parse: %v", err)
	}

	other, _ := domain.NewEnrollmentToken()
	if other.Key() == tok.Key() {
		t.Error("two tokens derive the same key")
	}

	for _, in := range []string{
		"short", "has space in the middle of it", string(make([]byte, 300)),
		"abcdefghijklmnopqrstuvwxyz",                   // 26 printable characters: too little entropy
		"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", // padded
		"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwd",     // 30 bytes
		"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh+",  // base64 alphabet, not base64url
	} {
		if _, err := domain.ParseEnrollmentToken(in); !errors.Is(err, domain.ErrInvalidEnrollmentToken) {
			t.Errorf("ParseEnrollmentToken(%q) = %v", in, err)
		}
	}

	k := tok.Key()
	nonce := []byte("nonce")
	id := domain.MustNodeID("attic")

	var h [32]byte

	p := k.HelloProof(nonce, id, h)
	if !domain.VerifyMAC(p, k.HelloProof(nonce, id, h)) || domain.VerifyMAC(p, other.Key().HelloProof(nonce, id, h)) ||
		domain.VerifyMAC(p, k.HelloProof(nonce, domain.MustNodeID("garden"), h)) {
		t.Error("hello proof does not bind key and node id")
	}

	if bytes.Equal(k.CSRMAC(nonce, []byte("a")), k.CSRMAC(nonce, []byte("b"))) {
		t.Error("CSR MAC does not bind the CSR")
	}

	// The MACs are domain-separated: a CSR MAC is never a valid chain MAC
	// for the same bytes, and chain boundaries matter.
	der := []byte("certificate")
	if bytes.Equal(k.CSRMAC(nonce, der), k.ChainMAC(nonce, [][]byte{der})) {
		t.Error("CSR and chain MACs are interchangeable")
	}

	if bytes.Equal(k.ChainMAC(nonce, [][]byte{[]byte("ab"), []byte("c")}), k.ChainMAC(nonce, [][]byte{[]byte("a"), []byte("bc")})) {
		t.Error("chain MAC ignores certificate boundaries")
	}

	if bytes.Equal(k.ChainMAC(nonce, [][]byte{[]byte("a")}), k.ChainMAC([]byte("other"), [][]byte{[]byte("a")})) {
		t.Error("chain MAC does not bind the nonce")
	}
}

func newPending(t *testing.T) (*domain.Node, domain.EnrollmentKey) {
	t.Helper()

	tok, _ := domain.NewEnrollmentToken()
	n := domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://x:1"), t0)
	n.IssueEnrollmentKey(tok.Key(), t0.Add(time.Hour), t0)

	return n, tok.Key()
}

func TestNodeEnrollmentLifecycle(t *testing.T) {
	n, key := newPending(t)
	cert, _ := domain.NewCertInfo(make([]byte, 32), "abc", t0.Add(90*24*time.Hour))

	if k, err := n.EnrollmentKey(t0.Add(time.Minute)); err != nil || k != key {
		t.Fatalf("key: %v", err)
	}

	if _, err := n.EnrollmentKey(t0.Add(time.Hour)); !errors.Is(err, domain.ErrEnrollmentTokenExpired) {
		t.Errorf("expired key: %v", err)
	}

	if err := n.CompleteEnrollment(key, n.URL(), cert, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	if n.Enrollment() != domain.EnrollmentEnrolled || n.HasEnrollmentKey() || n.Certificate().Serial() != "ABC" || !n.Active() {
		t.Errorf("enrolled = %+v", n.Snapshot())
	}

	if err := n.CompleteEnrollment(key, n.URL(), cert, t0.Add(time.Minute)); !errors.Is(err, domain.ErrNodeNotPending) {
		t.Errorf("token reuse: %v", err)
	}

	old, err := n.Revoke(t0.Add(time.Hour))
	if err != nil || old != cert || n.Enrollment() != domain.EnrollmentRevoked || n.Active() {
		t.Fatalf("revoke: %v %+v", err, n.Snapshot())
	}

	if _, err := n.Revoke(t0); !errors.Is(err, domain.ErrNodeRevoked) {
		t.Errorf("second revoke: %v", err)
	}

	// Re-enrollment needs a new token.
	tok, _ := domain.NewEnrollmentToken()
	n.IssueEnrollmentKey(tok.Key(), time.Time{}, t0)

	if n.Enrollment() != domain.EnrollmentPending || !n.Certificate().IsZero() {
		t.Errorf("re-enroll = %+v", n.Snapshot())
	}
}

func TestNodeUpdate(t *testing.T) {
	cfg := domain.NewConfigNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://x:1"), nil, t0)
	name := domain.MustNodeName("Other")
	disabled := true

	if err := cfg.Update(domain.NodePatch{Name: &name}, cfg.Version(), t0); !errors.Is(err, domain.ErrSettingLocked) {
		t.Errorf("locked name: %v", err)
	}

	if err := cfg.Update(domain.NodePatch{Disabled: &disabled}, cfg.Version()+1, t0); !errors.Is(err, domain.ErrVersionConflict) {
		t.Errorf("stale version: %v", err)
	}

	v := cfg.Version()
	if err := cfg.Update(domain.NodePatch{Disabled: &disabled}, v, t0); err != nil || !cfg.Disabled() || cfg.Version() != v+1 {
		t.Errorf("disable: %v", err)
	}

	if err := cfg.CheckDeletable(); !errors.Is(err, domain.ErrEntityLocked) {
		t.Errorf("delete config node: %v", err)
	}

	if !cfg.ReleaseFromConfig(t0) || cfg.CheckDeletable() != nil {
		t.Error("released node must be deletable")
	}

	if err := cfg.Update(domain.NodePatch{Name: &name}, cfg.Version(), t0); err != nil || cfg.Name() != name {
		t.Errorf("rename released node: %v", err)
	}
}

func TestRehydrateRoundTrip(t *testing.T) {
	n, _ := newPending(t)
	n.SetStatus(domain.StatusDegraded, "x")

	back, err := domain.RehydrateNode(n.Snapshot())
	if err != nil {
		t.Fatal(err)
	}

	if back.Version() != n.Version() || back.Runtime().Status != domain.StatusDegraded || !back.HasEnrollmentKey() {
		t.Errorf("round trip = %+v", back.Snapshot())
	}

	s := n.Snapshot()
	s.Enrollment = "bogus"

	if _, err := domain.RehydrateNode(s); err == nil {
		t.Error("invalid snapshot accepted")
	}
}
