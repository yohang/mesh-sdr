package media_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

// Revocations carry the hub time: tokens issued before it are refused,
// tokens of a later sign-in are not, whatever the node clock and however
// often the hub pushes the entry again. A second revocation of the same
// user refuses the tokens issued between the two.
func TestRevocationTimes(t *testing.T) {
	e := newEnv(t)
	e.installKeys(t, e.key)

	exp := time.Now().Add(5 * time.Minute)
	revokedAt := time.Now().Add(-time.Minute)

	issued := func(at time.Time) func(*token.Claims) {
		return func(c *token.Claims) { c.IssuedAt, c.NotBefore = at, at.Add(-time.Second) }
	}

	first := ctl.Revocations{Users: []ctl.Revoked{{ID: "u1", At: revokedAt.UnixMilli()}}}
	e.srv.Revoke(first)

	// A re-push of the same entry (a reconnect) changes nothing.
	e.srv.Revoke(first)

	if _, st := e.dial(t, e.client, e.token(t, "c1", exp, issued(revokedAt.Add(-5*time.Second))), "c1", origin); st != http.StatusForbidden {
		t.Fatalf("token issued before the revocation: status %d, want 403", st)
	}

	if _, st := e.dial(t, e.client, e.token(t, "c2", exp, issued(revokedAt.Add(5*time.Second))), "c2", origin); st != http.StatusSwitchingProtocols {
		t.Fatalf("token of a later sign-in: status %d, want 101", st)
	}

	// A second revocation of the same user, then the first pushed again.
	secondAt := revokedAt.Add(30 * time.Second)
	e.srv.Revoke(ctl.Revocations{Users: []ctl.Revoked{{ID: "u1", At: secondAt.UnixMilli()}}})
	e.srv.Revoke(first)

	if _, st := e.dial(t, e.client, e.token(t, "c3", exp, issued(revokedAt.Add(10*time.Second))), "c3", origin); st != http.StatusForbidden {
		t.Fatalf("token issued between the two revocations: status %d, want 403", st)
	}

	if _, st := e.dial(t, e.client, e.token(t, "c4", exp, issued(secondAt.Add(5*time.Second))), "c4", origin); st != http.StatusSwitchingProtocols {
		t.Fatalf("token issued after the second revocation: status %d, want 101", st)
	}
}
