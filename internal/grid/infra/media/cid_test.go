package media_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

// A cid serves one connection only, even after that connection closed.
func TestCIDSingleUse(t *testing.T) {
	e := newEnv(t)
	e.installKeys(t, e.key)

	tok := e.token(t, "c1", time.Now().Add(5*time.Minute))

	ws, st := e.dial(t, e.client, tok, "c1", origin)
	if st != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", st)
	}

	_ = ws.CloseNow()

	deadline := time.Now().Add(2 * time.Second)
	for e.srv.Count() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if _, st := e.dial(t, e.client, tok, "c1", origin); st != http.StatusConflict {
		t.Fatalf("replayed cid: status %d, want 409", st)
	}
}

// auth.refresh keeps the holder: same subject and session, except an
// anonymous connection whose visitor signed in.
func TestRefreshHolder(t *testing.T) {
	e := newEnv(t)
	e.installKeys(t, e.key)

	exp := time.Now().Add(5 * time.Minute)

	refresh := func(t *testing.T, cid string, first, next func(*token.Claims)) string {
		t.Helper()

		ws, st := e.dial(t, e.client, e.token(t, cid, exp, first), cid, origin)
		if st != http.StatusSwitchingProtocols {
			t.Fatalf("status %d", st)
		}

		hello(t, ws)
		send(t, ws, rxv1.TypeAuthRefresh, "r1", map[string]string{"token": e.token(t, cid, exp.Add(time.Minute), next)})

		env, err := read(t, ws)
		if err != nil {
			t.Fatal(err)
		}

		return string(env.Type()) + " " + string(env.Payload())
	}

	other := func(c *token.Claims) { c.Subject = "u2" }
	otherSession := func(c *token.Claims) { c.SessionID = token.SessionRef("s2") }
	anon := func(c *token.Claims) { c.Subject, c.SessionID = token.AnonymousSubject, "" }
	same := func(*token.Claims) {}

	if got := refresh(t, "c1", same, other); !strings.Contains(got, "token_invalid") {
		t.Errorf("another user: %s", got)
	}

	if got := refresh(t, "c2", same, otherSession); !strings.Contains(got, "token_invalid") {
		t.Errorf("another session: %s", got)
	}

	if got := refresh(t, "c3", anon, same); !strings.HasPrefix(got, "ack") {
		t.Errorf("anonymous visitor signed in: %s", got)
	}

	if got := refresh(t, "c4", same, same); !strings.HasPrefix(got, "ack") {
		t.Errorf("same holder: %s", got)
	}
}
